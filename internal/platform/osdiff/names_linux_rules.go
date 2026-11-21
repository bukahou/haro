package osdiff

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bukahou/haro/internal/terminal"
)

// 这个文件不带构建标签（文件名不以 _linux.go 结尾）：Linux 上挑摄像头的规则在任何机器上都能测。

// v4l2Node 是一个 /dev/video* 节点按 V4L2 查到的信息（查法见 probeV4L2Node）。
type v4l2Node struct {
	path    string
	name    string // VIDIOC_QUERYCAP 报的设备名
	capture bool   // 能当视频采集设备打开，并且列得出视频采集格式
}

// cameraNodes 挑出能拍照的节点，按设备号排。一个摄像头常有好几个节点（UVC 摄像头另有一个元数据节点），
// 只有能列出视频采集格式的那个是拍照用的。
func cameraNodes(nodes []v4l2Node) []terminal.Device {
	nodes = slices.Clone(nodes)
	slices.SortFunc(nodes, func(a, b v4l2Node) int { return videoNumber(a.path) - videoNumber(b.path) })
	var cameras []terminal.Device
	for _, node := range nodes {
		if node.capture {
			cameras = append(cameras, terminal.Device{ID: node.path, Name: node.name, Bus: terminal.BusUnknown})
		}
	}
	return cameras
}

func videoNumber(path string) int {
	number, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "video"))
	if err != nil {
		return 1 << 30
	}
	return number
}
