package smartturn

import (
	"errors"
	"fmt"
	"os"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// Detector 用 Smart Turn v3 的 ONNX 模型判断一段语音在语气上说完没有。
//
// ⚠️ 用 cgo 调 onnxruntime，只允许 cmd/haro-asr 引用。onnxruntime 动态库用 sherpa-onnx 自带的那份
// （1.28.2，进程启动时已经随 sherpa 加载），所以 onnxruntime_go 固定在 v1.32.1（API 28）；
// 升级 sherpa-onnx 时，两者的版本要一起对齐。
type Detector struct {
	mu         sync.Mutex // 特征提取器与会话都按串行使用
	features   *FeatureExtractor
	session    *ort.DynamicAdvancedSession
	inputName  string
	outputName string
}

// NewDetector 加载模型。libPath 是 onnxruntime 动态库（默认 "libonnxruntime.so"，即已随 sherpa 加载的那份）。
func NewDetector(modelPath, libPath string) (*Detector, error) {
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("Smart Turn 模型不可用: %w", err)
	}
	if !ort.IsInitialized() {
		ort.SetSharedLibraryPath(libPath)
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("初始化 onnxruntime（%s）: %w", libPath, err)
		}
	}
	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("读取模型输入输出: %w", err)
	}
	if len(inputs) != 1 || len(outputs) != 1 {
		return nil, fmt.Errorf("模型应有 1 个输入 1 个输出，实际 %d / %d", len(inputs), len(outputs))
	}
	options, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer options.Destroy()
	if err := setThreads(options); err != nil {
		return nil, err
	}
	session, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{inputs[0].Name}, []string{outputs[0].Name}, options)
	if err != nil {
		return nil, fmt.Errorf("创建推理会话: %w", err)
	}
	return &Detector{features: NewFeatureExtractor(), session: session,
		inputName: inputs[0].Name, outputName: outputs[0].Name}, nil
}

// sessionThreads 是会话选项里设推理线程数的那部分（*ort.SessionOptions 满足它）。
type sessionThreads interface {
	SetIntraOpNumThreads(n int) error
	SetInterOpNumThreads(n int) error
}

// setThreads 设推理线程数：算子内 2 个、算子间 1 个（定值，没有单独实测）。设不上就报错、起不来：
// 不报的话 onnxruntime 悄悄按默认线程数（所有核）跑，和识别抢 CPU，也没有日志。
func setThreads(options sessionThreads) error {
	if err := options.SetIntraOpNumThreads(2); err != nil {
		return fmt.Errorf("设 Smart Turn 的推理线程数（算子内）: %w", err)
	}
	if err := options.SetInterOpNumThreads(1); err != nil {
		return fmt.Errorf("设 Smart Turn 的推理线程数（算子间）: %w", err)
	}
	return nil
}

// EndOfTurnProbability 返回「已经说完」的概率（0–1）。samples 是 16kHz 单声道，取最后 8 秒。
func (d *Detector) EndOfTurnProbability(samples []float32) (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	features := d.features.Compute(PrepareWindow(samples))
	input, err := ort.NewTensor(ort.NewShape(1, nMels, nFrames), features)
	if err != nil {
		return 0, err
	}
	defer input.Destroy()
	outputs := []ort.Value{nil} // 由 onnxruntime 分配输出
	if err := d.session.Run([]ort.Value{input}, outputs); err != nil {
		return 0, fmt.Errorf("Smart Turn 推理: %w", err)
	}
	defer outputs[0].Destroy()
	tensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok || len(tensor.GetData()) == 0 {
		return 0, errors.New("Smart Turn 输出格式不符合预期")
	}
	return float64(tensor.GetData()[0]), nil
}

// Close 释放推理会话。
func (d *Detector) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.session.Destroy()
}
