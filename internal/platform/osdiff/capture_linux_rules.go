package osdiff

import (
	"fmt"
	"strconv"
	"strings"
)

// 这个文件不带构建标签（文件名不以 _linux.go 结尾）：Linux 上「向摄像头要什么格式」的规则在任何机器上都能测。
//
// V4L2 摄像头每种格式支持的分辨率不同（例如 EMEET C960：未压缩的 YUYV 最大 640×480，MJPEG 才有 1280×720），
// 而 ffmpeg 不指定格式时用驱动的默认格式；要的分辨率那个格式没有，驱动会悄悄换成它有的，ffmpeg 不报错。
// 所以先按 V4L2 列出摄像头支持的格式、分辨率和帧率（listV4L2Formats），再明确要一个。

// v4l2Format 是摄像头支持的一种格式，及它能出的分辨率。
type v4l2Format struct {
	name       string // ffmpeg 的 -input_format 名，如 mjpeg、yuyv422
	compressed bool
	modes      []frameMode
	ranges     []frameSizeRange // 有的驱动报连续 / 步进的范围，而不是一个个分辨率
}

// frameMode 是一个分辨率和它最高的帧率（不知道时为 0）。
type frameMode struct {
	size frameSize
	fps  float64
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

// v4l2FourCC 是 V4L2 像素格式的四字符码（内核 videodev2.h 的 v4l2_fourcc）。
func v4l2FourCC(code string) uint32 {
	return uint32(code[0]) | uint32(code[1])<<8 | uint32(code[2])<<16 | uint32(code[3])<<24
}

// ffmpegPixelFormat 是一种 V4L2 像素格式在 ffmpeg 的 v4l2 输入里叫什么、是不是压缩的。
type ffmpegPixelFormat struct {
	name       string
	compressed bool
}

// ffmpegPixelFormats：ffmpeg 的 v4l2 输入认得的像素格式；别的格式不向摄像头要。
var ffmpegPixelFormats = map[uint32]ffmpegPixelFormat{
	v4l2FourCC("MJPG"): {"mjpeg", true},
	v4l2FourCC("JPEG"): {"mjpeg", true},
	v4l2FourCC("H264"): {"h264", true},
	v4l2FourCC("YUYV"): {"yuyv422", false},
	v4l2FourCC("UYVY"): {"uyvy422", false},
	v4l2FourCC("NV12"): {"nv12", false},
	v4l2FourCC("YU12"): {"yuv420p", false},
}

// chooseV4L2Capture 选向摄像头要的格式与分辨率：
//  1. 有格式能出想要的分辨率 → 就要它；
//  2. 都出不了 → 宽度够（≥ 想要的宽）的里面最小的：够用就好，分辨率越大解码越费 CPU；
//  3. 都不够宽 → 最大的那个。
//
// 同一个分辨率几种格式都能出时：帧率高的优先（帧率低的出第一帧慢，例如未压缩格式在高分辨率下常只有几帧），
// 一样高时未压缩的优先（之后还要压成 JPEG，少压一次）。
// 列不出格式、或想要的分辨率写法认不出时，format 为空：照旧只给分辨率，由驱动定格式。
func chooseV4L2Capture(formats []v4l2Format, wanted string) (format, size string) {
	want, ok := parseFrameSize(wanted)
	if !ok || len(formats) == 0 {
		return "", wanted
	}
	type candidate struct {
		format string
		size   frameSize
		fps    float64
		raw    bool
	}
	var candidates []candidate
	for _, f := range formats {
		for _, m := range f.modes {
			candidates = append(candidates, candidate{f.name, m.size, m.fps, !f.compressed})
		}
		for _, r := range f.ranges {
			if r.contains(want) {
				candidates = append(candidates, candidate{f.name, want, 0, !f.compressed})
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
		if a.fps != b.fps {
			return a.fps > b.fps
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
