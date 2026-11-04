package audio

// WakeWordDetector 由可部署的关键字模型实现；未配置模型时必须显式禁用，不能用 VAD 冒充唤醒词识别。
type WakeWordDetector interface {
	Process(frame []byte) (bool, error)
}

type DisabledDetector struct{}

func (DisabledDetector) Process([]byte) (bool, error) {
	return false, nil
}
