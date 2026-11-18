package osdiff

import (
	"reflect"
	"testing"
)

// c960Formats 是 EMEET C960 的实际输出（ffmpeg 8.0）：未压缩的只到 640×480，1280×720 只有 MJPEG。
const c960Formats = `[video4linux2,v4l2 @ 0x58c33e708e40] Compressed:       mjpeg :          Motion-JPEG : 1920x1080 1280x960 1280x720 1024x576 800x600 640x480 640x360
[video4linux2,v4l2 @ 0x58c33e708e40] Raw       :     yuyv422 :           YUYV 4:2:2 : 640x480 640x360
Error opening input file /dev/video0.`

func TestParseV4L2Formats(t *testing.T) {
	got := parseV4L2Formats(c960Formats)
	if len(got) != 2 || got[0].name != "mjpeg" || !got[0].compressed || len(got[0].sizes) != 7 ||
		got[1].name != "yuyv422" || got[1].compressed || !reflect.DeepEqual(got[1].sizes, []frameSize{{640, 480}, {640, 360}}) {
		t.Fatalf("C960 的格式解析不对：%+v", got)
	}

	// 有的驱动报步进范围；ffmpeg 不认识的格式写 Unsupported；模拟出来的格式多一段「Emulated :」
	other := `[video4linux2,v4l2 @ 0x1] Raw       :     yuv420p :     Planar YUV 4:2:0 : {32-2592, 2}x{32-1944, 2}
[video4linux2,v4l2 @ 0x1] Raw       : Unsupported :                 XYZW : 640x480
[video4linux2,v4l2 @ 0x1] Raw       :       bgr24 :            BGR24 : Emulated : 320x240`
	got = parseV4L2Formats(other)
	if len(got) != 2 || got[0].name != "yuv420p" || len(got[0].sizes) != 0 ||
		got[0].ranges[0] != (frameSizeRange{32, 2592, 2, 32, 1944, 2}) ||
		got[1].name != "bgr24" || !reflect.DeepEqual(got[1].sizes, []frameSize{{320, 240}}) {
		t.Fatalf("范围 / Unsupported / Emulated 解析不对：%+v", got)
	}

	if got := parseV4L2Formats("ffmpeg: command not found"); len(got) != 0 {
		t.Errorf("没有格式清单时应为空：%+v", got)
	}
}

func TestChooseV4L2Capture(t *testing.T) {
	c960 := parseV4L2Formats(c960Formats)
	both := []v4l2Format{
		{name: "mjpeg", compressed: true, sizes: []frameSize{{1280, 720}, {640, 480}}},
		{name: "yuyv422", sizes: []frameSize{{1280, 720}, {640, 480}}},
	}
	big := []v4l2Format{{name: "mjpeg", compressed: true, sizes: []frameSize{{3280, 2464}, {1920, 1080}, {1640, 1232}}}}
	small := []v4l2Format{
		{name: "mjpeg", compressed: true, sizes: []frameSize{{320, 240}}},
		{name: "yuyv422", sizes: []frameSize{{640, 480}, {320, 240}}},
	}
	stepwise := parseV4L2Formats(`[v @ 0x1] Raw       :     yuv420p :     Planar YUV 4:2:0 : {32-2592, 2}x{32-1944, 2}`)
	cases := []struct {
		name               string
		formats            []v4l2Format
		wanted             string
		wantFormat, wantSz string
	}{
		{"C960：1280x720 只有 MJPEG 能出", c960, "1280x720", "mjpeg", "1280x720"},
		{"C960：640x480 两种都能出，要未压缩的", c960, "640x480", "yuyv422", "640x480"},
		{"同一分辨率两种格式都能出，要未压缩的", both, "1280x720", "yuyv422", "1280x720"},
		{"没有想要的：宽度够的里面要最小的", big, "1280x720", "mjpeg", "1640x1232"},
		{"都不够宽：要最大的", small, "1280x720", "yuyv422", "640x480"},
		{"步进范围里有想要的", stepwise, "1280x720", "yuv420p", "1280x720"},
		{"在范围里但对不上步进：不算有", stepwise, "1281x721", "", "1281x721"},
		{"列不出格式：照旧由驱动定", nil, "1280x720", "", "1280x720"},
		{"分辨率写法认不出：照原样", c960, "hd720", "", "hd720"},
	}
	for _, c := range cases {
		if format, size := chooseV4L2Capture(c.formats, c.wanted); format != c.wantFormat || size != c.wantSz {
			t.Errorf("%s：得到 %s %s，应为 %s %s", c.name, format, size, c.wantFormat, c.wantSz)
		}
	}
}

func TestV4L2Capture(t *testing.T) {
	got := v4l2Capture("/dev/video0", parseV4L2Formats(c960Formats), "1280x720")
	want := CameraCapture{Input: []string{"-f", "v4l2", "-input_format", "mjpeg", "-video_size", "1280x720", "-i", "/dev/video0"},
		Format: "mjpeg", Size: "1280x720"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("C960 的输入参数不对：%+v", got)
	}
	if got := v4l2Capture("/dev/video0", nil, "1280x720"); !reflect.DeepEqual(got.Input,
		[]string{"-f", "v4l2", "-video_size", "1280x720", "-i", "/dev/video0"}) || got.Format != "" {
		t.Errorf("列不出格式时应不指定格式：%+v", got)
	}
}
