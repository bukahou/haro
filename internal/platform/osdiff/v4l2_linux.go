//go:build linux

package osdiff

import (
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/blackjack/webcam"
)

// v4l2Probe 按 V4L2 查设备节点，结果按节点缓存：节点没换（inode 没变）就不再打开设备
// （设备清单每隔几秒查一次；摄像头拔掉再插上时节点会重新建，inode 跟着变，就重新查）。
type v4l2Probe struct {
	mu    sync.Mutex
	nodes map[string]probedNode // 键：设备节点路径
}

type probedNode struct {
	inode uint64
	node  v4l2Node
}

// node 返回 path 的查询结果（节点没变时用缓存的）。
func (p *v4l2Probe) node(path string) v4l2Node {
	var inode uint64
	if info, err := os.Stat(path); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			inode = stat.Ino
		}
	}
	p.mu.Lock()
	cached, ok := p.nodes[path]
	p.mu.Unlock()
	if ok && inode != 0 && cached.inode == inode {
		return cached.node
	}
	node := probeV4L2Node(path)
	p.mu.Lock()
	if p.nodes == nil {
		p.nodes = map[string]probedNode{}
	}
	p.nodes[path] = probedNode{inode: inode, node: node}
	p.mu.Unlock()
	return node
}

// probeV4L2Node 查一个设备节点：能当视频采集设备打开（VIDIOC_QUERYCAP 报能采集、能流式读），并且列得出视频采集格式
// （VIDIOC_ENUM_FMT）才算摄像头 —— UVC 摄像头的元数据节点也报「能采集」（QUERYCAP 报的是整个设备的能力），
// 但列不出视频格式。只查询，不开始采集。
func probeV4L2Node(path string) v4l2Node {
	device, err := webcam.OpenWithCloseOnExec(path)
	if err != nil {
		return v4l2Node{path: path}
	}
	defer device.Close()
	name, _ := device.GetName()
	return v4l2Node{path: path, name: strings.TrimSpace(name), capture: len(device.GetSupportedFormats()) > 0}
}

// listV4L2Formats 按 V4L2 列出摄像头支持的格式、分辨率和每个分辨率最高的帧率（只列 ffmpeg 认得的格式）。
// 打不开设备时返回空（由驱动定格式）。
func listV4L2Formats(path string) []v4l2Format {
	device, err := webcam.OpenWithCloseOnExec(path)
	if err != nil {
		return nil
	}
	defer device.Close()
	var formats []v4l2Format
	for code := range device.GetSupportedFormats() {
		known, ok := ffmpegPixelFormats[uint32(code)]
		if !ok {
			continue
		}
		format := v4l2Format{name: known.name, compressed: known.compressed}
		for _, size := range device.GetSupportedFrameSizes(code) {
			if size.StepWidth == 0 && size.StepHeight == 0 {
				format.modes = append(format.modes, frameMode{size: frameSize{int(size.MaxWidth), int(size.MaxHeight)},
					fps: highestFPS(device.GetSupportedFramerates(code, size.MaxWidth, size.MaxHeight))})
				continue
			}
			format.ranges = append(format.ranges, frameSizeRange{int(size.MinWidth), int(size.MaxWidth), int(size.StepWidth),
				int(size.MinHeight), int(size.MaxHeight), int(size.StepHeight)})
		}
		if len(format.modes) > 0 || len(format.ranges) > 0 {
			formats = append(formats, format)
		}
	}
	slices.SortFunc(formats, func(a, b v4l2Format) int { return strings.Compare(a.name, b.name) }) // 驱动报的顺序不固定
	return formats
}

// highestFPS 是一组帧间隔里最高的帧率：帧间隔是 numerator/denominator 秒，最短的间隔是 MinNumerator/MinDenominator。
func highestFPS(rates []webcam.FrameRate) float64 {
	highest := 0.0
	for _, rate := range rates {
		if rate.MinNumerator > 0 {
			highest = max(highest, float64(rate.MinDenominator)/float64(rate.MinNumerator))
		}
	}
	return highest
}
