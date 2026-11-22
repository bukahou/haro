package smartturn

import (
	"errors"
	"testing"
)

// threadSettings 记下设了几个线程，failIntra / failInter 为 true 时那一项设不上。
type threadSettings struct {
	failIntra, failInter bool
	intra, inter         int
}

var errNotSupported = errors.New("设不上")

func (s *threadSettings) SetIntraOpNumThreads(n int) error {
	if s.failIntra {
		return errNotSupported
	}
	s.intra = n
	return nil
}

func (s *threadSettings) SetInterOpNumThreads(n int) error {
	if s.failInter {
		return errNotSupported
	}
	s.inter = n
	return nil
}

// 推理线程数设不上时报错（起不来），不悄悄按默认线程数（所有核）跑；设得上时两项都设。
func TestSetThreads(t *testing.T) {
	for _, settings := range []*threadSettings{{failIntra: true}, {failInter: true}} {
		if err := setThreads(settings); !errors.Is(err, errNotSupported) {
			t.Errorf("线程数设不上应报错：%+v → %v", settings, err)
		}
	}
	settings := &threadSettings{}
	if err := setThreads(settings); err != nil || settings.intra != 2 || settings.inter != 1 {
		t.Errorf("应设算子内 2 个、算子间 1 个：%+v %v", settings, err)
	}
}
