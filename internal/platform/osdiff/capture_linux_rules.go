package osdiff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 这个文件不带构建标签（文件名不以 _linux.go 结尾）：Linux 上「向摄像头要什么格式」的规则在任何机器上都能测。
//
// V4L2 摄像头每种格式支持的分辨率不同（例如 EMEET C960：未压缩的 YUYV 最大 640×480，MJPEG 才有 1280×720），
// 而 ffmpeg 不指定格式时用驱动的默认格式；要的分辨率那个格式没有，驱动会悄悄换成它有的，ffmpeg 不报错。
// 所以先列出摄像头支持的格式与分辨率，再明确要一个。

// v4l2Format 是摄像头支持的一种格式，及它能出的分辨率。
type v4l2Format struct {
	name       string // ffmpeg 的 -input_format 名，如 mjpeg、yuyv422
	compressed bool
	sizes      []frameSize
	ranges     []frameSizeRange // 有的驱动报连续 / 步进的范围，而不是一个个分辨率
}

type frameSize struct{ width, height int }

func (s frameSize) String() string { return fmt.Sprintf("%dx%d", s.width, s.height) }

type frameSizeRange struct{ minWidth, maxWidth, stepWidth, minHeight, maxHeight, stepHeight int }

func (r frameSizeRange) contains(s frameSize) bool {
	inside := func(value, low, high, step int) bool {
		return value >= low && value <= high && (step <= 1 || (value-low)%step == 0)
	}
	return inside(s.width, r.minWidth, r.maxWidth, r.stepWidth) && inside(s.height, r.minHeight, r.maxHeight, r.stepHeight)
}

var (
	frameSizePattern  = regexp.MustCompile(`(\d+)x(\d+)`)
	frameRangePattern = regexp.MustCompile(`\{(\d+)-(\d+), ?(\d+)\}x\{(\d+)-(\d+), ?(\d+)\}`)
)

// parseV4L2Formats 解析 `ffmpeg -f v4l2 -list_formats all -i <设备>` 的输出，每种格式一行：
//
//	[video4linux2,v4l2 @ 0x…] Compressed:       mjpeg :          Motion-JPEG : 1920x1080 1280x720 640x480
//	[video4linux2,v4l2 @ 0x…] Raw       :     yuyv422 :           YUYV 4:2:2 : 640x480 640x360
//
// 格式说明里也可能有冒号（YUYV 4:2:2），所以按「 : 」分段；ffmpeg 不认识的格式（Unsupported）跳过。
func parseV4L2Formats(output string) []v4l2Format {
	var formats []v4l2Format
	for _, line := range strings.Split(output, "\n") {
		_, rest, found := strings.Cut(line, "] ")
		if !found {
			continue
		}
		kind, fields, found := strings.Cut(rest, ":")
		kind = strings.TrimSpace(kind)
		if !found || (kind != "Raw" && kind != "Compressed") {
			continue
		}
		parts := strings.Split(fields, " : ")
		name := strings.TrimSpace(parts[0])
		if len(parts) < 3 || name == "" || name == "Unsupported" {
			continue
		}
		format := v4l2Format{name: name, compressed: kind == "Compressed"}
		sizes := parts[len(parts)-1]
		for _, match := range frameRangePattern.FindAllStringSubmatch(sizes, -1) {
			format.ranges = append(format.ranges, frameSizeRange{atoi(match[1]), atoi(match[2]), atoi(match[3]),
				atoi(match[4]), atoi(match[5]), atoi(match[6])})
		}
		for _, match := range frameSizePattern.FindAllStringSubmatch(frameRangePattern.ReplaceAllString(sizes, ""), -1) {
			format.sizes = append(format.sizes, frameSize{atoi(match[1]), atoi(match[2])})
		}
		if len(format.sizes) > 0 || len(format.ranges) > 0 {
			formats = append(formats, format)
		}
	}
	return formats
}

// chooseV4L2Capture 选向摄像头要的格式与分辨率：
//  1. 有格式能出想要的分辨率 → 就要它；
//  2. 都出不了 → 宽度够（≥ 想要的宽）的里面最小的：够用就好，分辨率越大解码越费 CPU；
//  3. 都不够宽 → 最大的那个。
//
// 同一个分辨率两种格式都能出时，未压缩的优先（之后还要压成 JPEG，少压一次）。
// 列不出格式、或想要的分辨率写法认不出时，format 为空：照旧只给分辨率，由驱动定格式。
func chooseV4L2Capture(formats []v4l2Format, wanted string) (format, size string) {
	want, ok := parseFrameSize(wanted)
	if !ok || len(formats) == 0 {
		return "", wanted
	}
	type candidate struct {
		format string
		size   frameSize
		raw    bool
	}
	var candidates []candidate
	for _, f := range formats {
		for _, s := range f.sizes {
			candidates = append(candidates, candidate{f.name, s, !f.compressed})
		}
		for _, r := range f.ranges {
			if r.contains(want) {
				candidates = append(candidates, candidate{f.name, want, !f.compressed})
			}
		}
	}
	if len(candidates) == 0 {
		return "", wanted
	}
	area := func(s frameSize) int { return s.width * s.height }
	better := func(a, b candidate) bool { // a 比 b 更合适
		aExact, bExact := a.size == want, b.size == want
		if aExact != bExact {
			return aExact
		}
		aWide, bWide := a.size.width >= want.width, b.size.width >= want.width
		if aWide != bWide {
			return aWide
		}
		if area(a.size) != area(b.size) {
			return (area(a.size) < area(b.size)) == aWide // 够宽的取小，不够宽的取大
		}
		return a.raw && !b.raw
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if better(c, best) {
			best = c
		}
	}
	return best.format, best.size.String()
}

// v4l2Capture 是 Linux 上 ffmpeg 的摄像头输入：formats 是摄像头支持的格式（列不出时为空）。
func v4l2Capture(id string, formats []v4l2Format, wanted string) CameraCapture {
	format, size := chooseV4L2Capture(formats, wanted)
	input := []string{"-f", "v4l2"}
	if format != "" {
		input = append(input, "-input_format", format)
	}
	input = append(input, "-video_size", size, "-i", id)
	return CameraCapture{Input: input, Format: format, Size: size}
}

func parseFrameSize(text string) (frameSize, bool) {
	width, height, found := strings.Cut(text, "x")
	w, errW := strconv.Atoi(width)
	h, errH := strconv.Atoi(height)
	if !found || errW != nil || errH != nil || w <= 0 || h <= 0 {
		return frameSize{}, false
	}
	return frameSize{w, h}, true
}

func atoi(text string) int {
	number, _ := strconv.Atoi(text)
	return number
}
