// Package attachments 是 App 里发的图片：解码（JPEG / PNG / WebP）、长边缩到上限以内、统一存成 JPEG（模型的适配层只收 JPEG），
// 按编号存成文件、取回。谁的、哪条对话的记在对话库里（conversationstore 的 attachments 表）；这里只管图片本身。
package attachments

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // 注册 PNG 解码
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // 注册 WebP 解码
)

// 存下来的图片的设置。
const (
	// MaxSide 是长边最多多少像素（更大的等比缩小）：模型看图的效果到这里就够了，再大只多花 token。
	MaxSide = 1568
	// jpegQuality 是存成 JPEG 的质量。
	jpegQuality = 85
	// maxPixels 是解码前允许的最大像素数（宽 × 高）：挡住宽高写得极大、解码时吃光内存的图。
	maxPixels = 50_000_000
)

// ErrUnsupported 是图片解不开或格式不认识（App 的接口回 400）。
var ErrUnsupported = errors.New("图片格式不认识或解不开（只收 JPEG、PNG、WebP）")

// Normalize 把上传的图片解码、缩到长边不超过 MaxSide、存成 JPEG，返回 JPEG 和宽高。
func Normalize(data []byte) (encoded []byte, width, height int, err error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width*config.Height > maxPixels {
		return nil, 0, 0, fmt.Errorf("%w: 尺寸 %dx%d 不行", ErrUnsupported, config.Width, config.Height)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	bounds := decoded.Bounds()
	width, height = bounds.Dx(), bounds.Dy()
	if longest := max(width, height); longest > MaxSide {
		width, height = max(1, width*MaxSide/longest), max(1, height*MaxSide/longest)
		scaled := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(scaled, scaled.Bounds(), decoded, bounds, draw.Over, nil)
		decoded = scaled
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, decoded, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, 0, 0, fmt.Errorf("存成 JPEG: %w", err)
	}
	return out.Bytes(), width, height, nil
}

// Files 是存图片的目录（只有自己能读）。
type Files struct{ dir string }

// idPattern 是附件编号的写法（随机的十六进制）：拼文件名之前先核对，路径里不会出现别的字。
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// OpenFiles 打开（没有就建）存图片的目录。
func OpenFiles(dir string) (*Files, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("建存图片的目录 %s: %w", dir, err)
	}
	return &Files{dir: dir}, nil
}

func (f *Files) path(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("附件编号 %q 的写法不对", id)
	}
	return filepath.Join(f.dir, id+".jpg"), nil
}

// Write 存一张图片（先写临时文件再改名：写一半不会留下坏文件）。
func (f *Files) Write(id string, data []byte) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(f.dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("存图片: %w", err)
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("存图片: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("存图片: %w", err)
	}
	if err := os.Chmod(temp.Name(), 0o600); err != nil {
		return fmt.Errorf("存图片: %w", err)
	}
	return os.Rename(temp.Name(), path)
}

// Read 取回一张图片。
func (f *Files) Read(id string) ([]byte, error) {
	path, err := f.path(id)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Remove 删一张图片（没有也不算出错）。
func (f *Files) Remove(id string) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
