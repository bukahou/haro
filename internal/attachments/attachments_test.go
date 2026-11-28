package attachments

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func pngOf(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := range width {
		img.Set(x, 0, color.RGBA{R: 200, A: 255})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// PNG 转成 JPEG；长边超过上限的等比缩小，小的不放大；解不开的报 ErrUnsupported。
func TestNormalize(t *testing.T) {
	big, width, height, err := Normalize(pngOf(t, 3136, 1000))
	if err != nil || width != MaxSide || height != 500 {
		t.Fatalf("大图：%dx%d %v", width, height, err)
	}
	if _, err := jpeg.DecodeConfig(bytes.NewReader(big)); err != nil {
		t.Errorf("应存成 JPEG：%v", err)
	}
	if _, width, height, err := Normalize(pngOf(t, 200, 100)); err != nil || width != 200 || height != 100 {
		t.Errorf("小图不放大：%dx%d %v", width, height, err)
	}
	if _, _, _, err := Normalize([]byte("不是图片")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("解不开：%v", err)
	}
}

// 文件：存了取得回来、只有自己能读；编号写法不对的不碰文件系统；删掉后取不到，删两次不算错。
func TestFiles(t *testing.T) {
	files, err := OpenFiles(filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef"
	if err := files.Write(id, []byte("jpeg")); err != nil {
		t.Fatal(err)
	}
	if data, err := files.Read(id); err != nil || string(data) != "jpeg" {
		t.Errorf("取回：%q %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(files.dir, id+".jpg")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("权限：%v %v", info.Mode(), err)
	}
	for _, bad := range []string{"../x", "ABC", ""} {
		if _, err := files.Read(bad); err == nil {
			t.Errorf("%q 不该读得到", bad)
		}
	}
	if err := files.Remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Read(id); err == nil {
		t.Error("删掉后取不到")
	}
	if err := files.Remove(id); err != nil {
		t.Errorf("删两次：%v", err)
	}
}
