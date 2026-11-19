//go:build linux

package camera

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/mediadevices/pkg/driver"
	mdcamera "github.com/pion/mediadevices/pkg/driver/camera"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/prop"

	"github.com/bukahou/haro/internal/terminal"
)

// MediaDevicesSupported：这个系统上能用 mediadevices 拍照。
const MediaDevicesSupported = true

// FindDevice 找现在要用的摄像头：返回设备节点（例如 /dev/video0）和名字；ok=false 表示现在没有摄像头。
// 由平台层用服务层的选择规则组装；每次拍之前都调用，所以拔掉再插上、换了设备号都能接上。
type FindDevice func(ctx context.Context) (path, name string, ok bool)

// MediaDevicesCamera 用 pion/mediadevices 按需拍一帧：拍的时候才打开摄像头，预热到曝光稳定，取一帧后马上关上。
type MediaDevicesCamera struct {
	find        FindDevice
	captureSize terminal.FrameSize // 想向摄像头要的分辨率
	outputWidth int
	logger      *slog.Logger
	health      *health
	frameSize   *frameSizeLog

	mu        sync.Mutex // 一次只拍一张：摄像头同时只能被打开一次
	requested string     // 上次向摄像头要的格式：变了才写日志
}

// NewMediaDevicesCamera 创建拍照器（这时不打开摄像头）；name 是启动时选中的摄像头。
func NewMediaDevicesCamera(find FindDevice, name string, captureSize terminal.FrameSize, outputWidth int,
	logger *slog.Logger) (*MediaDevicesCamera, error) {
	return &MediaDevicesCamera{find: find, captureSize: captureSize, outputWidth: outputWidth, logger: logger,
		health: newHealth(name, logger), frameSize: newFrameSizeLog(logger)}, nil
}

// CurrentName 是现在能用的摄像头名字；没有时为空（看守每隔几秒问一次，据此告诉服务端）。
// 每次都按选择规则重新列一遍设备（Linux 上读系统目录，很快），所以启动时没插、之后插上的摄像头也能被发现。
func (c *MediaDevicesCamera) CurrentName() string {
	if _, name, ok := c.find(context.Background()); ok {
		return name
	}
	return ""
}

// Capture 拍一帧：打开摄像头，读帧直到曝光稳定（最多 warmupLimit），缩到不超过 maxWidth，编码成 JPEG，关上摄像头。
func (c *MediaDevicesCamera) Capture(ctx context.Context, maxWidth int) (terminal.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, name, ok := c.find(ctx)
	if !ok {
		c.health.markDown("没有找到摄像头", nil)
		return terminal.Frame{}, terminal.ErrNoCamera
	}
	device, err := mediaDevice(path)
	if err != nil {
		c.health.markDown("打不开摄像头", err)
		return terminal.Frame{}, err
	}
	img, release, warmup, err := c.readSettledFrame(ctx, device, name)
	if err != nil {
		c.health.markDown("拍不到画面", err)
		return terminal.Frame{}, err
	}
	jpeg, size, err := encodeJPEG(img, cmp.Or(maxWidth, c.outputWidth))
	release()
	if err != nil {
		return terminal.Frame{}, fmt.Errorf("编码画面: %w", err)
	}
	c.health.markUp(name)
	c.frameSize.observe(jpeg)
	c.logger.Debug("📷 拍了一帧", "device", name, "warmup_frames", warmup, "bytes", len(jpeg))
	return terminal.Frame{JPEG: jpeg, TakenAt: time.Now(), Width: size.X, Height: size.Y, Device: name}, nil
}

// readSettledFrame 打开设备、读帧直到曝光稳定，关上设备；返回那一帧和释放它的函数，以及读了几帧。
// ctx 结束或摄像头卡住时，关掉设备让阻塞的读取返回。
func (c *MediaDevicesCamera) readSettledFrame(ctx context.Context, device driver.Driver, name string) (image.Image, func(), int, error) {
	recorder, ok := device.(driver.VideoRecorder)
	if !ok {
		return nil, nil, 0, errors.New("这个设备不能拍画面")
	}
	if err := device.Open(); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil, nil, 0, fmt.Errorf("%w: %w", terminal.ErrCameraBusy, err)
		}
		return nil, nil, 0, fmt.Errorf("打开摄像头: %w", err)
	}
	var closeOnce sync.Once
	closeDevice := func() { closeOnce.Do(func() { _ = device.Close() }) }
	defer closeDevice()
	ctx, cancel := context.WithTimeout(ctx, warmupLimit+captureGrace)
	defer cancel()
	stop := context.AfterFunc(ctx, closeDevice) // 超时或被取消：关掉设备，正在等的那一帧随之返回错误
	defer stop()

	media, ok := chooseCaptureMedia(device.Properties(), c.captureSize)
	if !ok {
		return nil, nil, 0, errors.New("摄像头没有能用的画面格式")
	}
	if requested := fmt.Sprintf("%s %dx%d", media.FrameFormat, media.Width, media.Height); requested != c.requested {
		c.requested = requested
		c.logger.Info("📷 向摄像头要画面", "device", name, "format", media.FrameFormat,
			"size", fmt.Sprintf("%dx%d", media.Width, media.Height), "wanted", c.captureSize.String())
	}
	reader, err := recorder.VideoRecord(media)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("开始取画面: %w", err)
	}
	deadline := time.Now().Add(warmupLimit)
	var settler exposureSettler
	var latest image.Image
	release := func() {}
	for frames := 1; ; frames++ {
		img, releaseFrame, err := reader.Read()
		if err != nil {
			release()
			if ctx.Err() != nil {
				return nil, nil, frames, fmt.Errorf("等画面超时: %w", ctx.Err())
			}
			return nil, nil, frames, fmt.Errorf("读画面: %w", err)
		}
		release()
		latest, release = img, releaseFrame
		if settler.observe(meanLuma(img)) || !time.Now().Before(deadline) {
			return latest, release, frames, nil
		}
	}
}

// captureGrace：预热上限之外，再给打开设备、等第一帧留的时间。摄像头卡住时到点就放弃，不让这一轮一直等下去。
const captureGrace = 2 * time.Second

// mediaDevice 找到设备节点 path（或指向它的链接）在 mediadevices 里对应的设备；没找到时重新扫描一次（刚插上的摄像头）。
func mediaDevice(path string) (driver.Driver, error) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("摄像头 %s 不在了: %w", path, err)
	}
	node := filepath.Base(target)
	find := func() driver.Driver {
		for _, device := range driver.GetManager().Query(driver.FilterVideoRecorder()) {
			// 驱动给每个设备的标签是「发现时的名字;设备节点名」
			if _, deviceNode, found := strings.Cut(device.Info().Label, mdcamera.LabelSeparator); found && deviceNode == node {
				return device
			}
		}
		return nil
	}
	if device := find(); device != nil {
		return device, nil
	}
	mdcamera.Initialize()
	if device := find(); device != nil {
		return device, nil
	}
	return nil, fmt.Errorf("mediadevices 里找不到摄像头 %s", path)
}

// chooseCaptureMedia 从摄像头支持的格式里挑一个：分辨率正好是想要的优先；否则宽度够的里面挑最小的，都不够宽就挑最大的；
// 同样大小时优先压缩格式（MJPEG：高分辨率下帧率高，预热要连读好几帧），再挑帧率高的。
func chooseCaptureMedia(supported []prop.Media, want terminal.FrameSize) (prop.Media, bool) {
	ok := want.Width > 0 && want.Height > 0
	var best prop.Media
	found := false
	area := func(m prop.Media) int { return m.Width * m.Height }
	better := func(a, b prop.Media) bool { // a 比 b 更合适
		if ok {
			aExact, bExact := a.Width == want.Width && a.Height == want.Height, b.Width == want.Width && b.Height == want.Height
			if aExact != bExact {
				return aExact
			}
			aWide, bWide := a.Width >= want.Width, b.Width >= want.Width
			if aWide != bWide {
				return aWide
			}
			if area(a) != area(b) {
				return (area(a) < area(b)) == aWide // 够宽的取小，不够宽的取大
			}
		} else if area(a) != area(b) {
			return area(a) > area(b)
		}
		aCompressed, bCompressed := a.FrameFormat == frame.FormatMJPEG, b.FrameFormat == frame.FormatMJPEG
		if aCompressed != bCompressed {
			return aCompressed
		}
		return a.FrameRate > b.FrameRate
	}
	for _, media := range supported {
		if media.Width == 0 || media.Height == 0 {
			continue
		}
		if !found || better(media, best) {
			best, found = media, true
		}
	}
	return best, found
}
