package device

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"linx-intercom/server/internal/audio"
	"linx-intercom/server/internal/linx"
)

const sessionQueueSize = 32

// 每帧按模拟器的 20 ms 音频粒度估算，约 1 秒回看窗口用于覆盖唤醒短语及解码延迟。
const WakeWordLookbackFrames = 50

type Session struct {
	ID           string
	DeviceID     string
	UserID       string
	LinxClientID string

	AudioIn      chan []byte
	JanusAudioIn chan []byte
	AudioOut     chan downlinkMessage
	ControlIn    chan ControlMessage
	ControlOut   chan ControlMessage
	LinxAudioIn  chan []byte
	LinxEventsIn chan json.RawMessage
	LinxOut      chan linx.Result
	AudioRouter  *audio.Router
	Done         <-chan struct{}

	mu                    sync.Mutex
	conn                  *websocket.Conn
	generation            uint64
	timer                 *time.Timer
	cancel                context.CancelFunc
	mediaJoinMu           sync.Mutex
	mediaMu               sync.Mutex
	media                 *roomMediaBinding
	// 仅在本轮正常 TTS 停止后接受设备确认，避免重连重放的旧确认切回房间。
	awaitingPlaybackDrain atomic.Bool
}

func newSession(deviceID, userID string, queueSize int) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		ID:           newID(),
		DeviceID:     deviceID,
		UserID:       userID,
		AudioIn:      make(chan []byte, queueSize),
		JanusAudioIn: make(chan []byte, queueSize),
		AudioOut:     make(chan downlinkMessage, queueSize),
		ControlIn:    make(chan ControlMessage, queueSize),
		ControlOut:   make(chan ControlMessage, queueSize),
		LinxAudioIn:  make(chan []byte, queueSize),
		LinxEventsIn: make(chan json.RawMessage, queueSize),
		LinxOut:      make(chan linx.Result, queueSize),
		AudioRouter:  audio.NewRouter(audio.DisabledDetector{}, 15),
		Done:         ctx.Done(),
		cancel:       cancel,
	}
}

func (s *Session) EnqueueAudioIn(frame []byte) bool {
	dropped := enqueueAudio(s.AudioIn, frame)
	if dropped {
		return true
	}
	return false
}

func (s *Session) EnqueueAudioOut(frame []byte) bool {
	return enqueueLatest(s.AudioOut, downlinkMessage{audio: append([]byte(nil), frame...), mode: audio.ModeRoom})
}

// 正常 TTS stop 作为队尾标记随音频发送，避免另一条控制队列提前结束播放。
type downlinkMessage struct {
	audio []byte
	event *ControlMessage
	mode  audio.Mode
}

func (s *Session) ConnectionGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

func enqueueAudio(queue chan []byte, frame []byte) bool {
	return enqueueLatest(queue, append([]byte(nil), frame...))
}

func enqueueLatest[T any](queue chan T, frame T) bool {
	dropped := false
	select {
	case queue <- frame:
		return false
	default:
	}
	select {
	case <-queue:
		dropped = true
	default:
	}
	select {
	case queue <- frame:
	default:
		dropped = true
	}
	return dropped
}

func newID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(fmt.Sprintf("generate session id: %v", err))
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

type SessionManager struct {
	mu               sync.Mutex
	grace            time.Duration
	sessions         map[string]*Session
	userIDs          map[string]string
	clientIDs        map[string]string
	logger           *slog.Logger
	resolveUser      func(string) (string, error)
	resolveClientID  func(string) (string, error)
	wakeWordFactory  func() (audio.WakeWordDetector, error)
	wakeWordLookback int
	onCreate         func(*Session)
}

func NewSessionManager(grace time.Duration) *SessionManager {
	if grace <= 0 {
		grace = 60 * time.Second
	}
	return &SessionManager{
		grace:     grace,
		sessions:  make(map[string]*Session),
		userIDs:   make(map[string]string),
		clientIDs: make(map[string]string),
		wakeWordFactory: func() (audio.WakeWordDetector, error) {
			return audio.DisabledDetector{}, nil
		},
		wakeWordLookback: 1,
		logger:           slog.Default(),
	}
}

func (m *SessionManager) SetWakeWordFactory(factory func() (audio.WakeWordDetector, error), lookbackFrames int) {
	m.mu.Lock()
	if factory == nil {
		m.wakeWordFactory = func() (audio.WakeWordDetector, error) { return audio.DisabledDetector{}, nil }
		m.wakeWordLookback = 1
	} else {
		m.wakeWordFactory = factory
		m.wakeWordLookback = max(lookbackFrames, 1)
	}
	m.mu.Unlock()
}

func (m *SessionManager) SetLinxClientIDResolver(resolve func(string) (string, error)) {
	m.mu.Lock()
	m.resolveClientID = resolve
	m.mu.Unlock()
}

func (m *SessionManager) SetIdentityResolver(resolve func(string) (string, error)) {
	m.mu.Lock()
	m.resolveUser = resolve
	m.mu.Unlock()
}

func (m *SessionManager) SetOnCreate(onCreate func(*Session)) {
	m.mu.Lock()
	m.onCreate = onCreate
	m.mu.Unlock()
}

func (m *SessionManager) Attach(deviceID string, conn *websocket.Conn) (*Session, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session := m.sessions[deviceID]
	if session == nil {
		userID := m.userIDs[deviceID]
		if userID == "" {
			if m.resolveUser != nil {
				var err error
				userID, err = m.resolveUser(deviceID)
				if err != nil {
					return nil, 0, fmt.Errorf("resolve device user: %w", err)
				}
			} else {
				userID = "usr_" + newID()
			}
			m.userIDs[deviceID] = userID
		}
		clientID := m.clientIDs[deviceID]
		if clientID == "" && m.resolveClientID != nil {
			var err error
			clientID, err = m.resolveClientID(deviceID)
			if err != nil {
				return nil, 0, fmt.Errorf("resolve Linx client identity: %w", err)
			}
		}
		if clientID == "" {
			clientID = newID()
		}
		// 先解析持久身份，再加载原生 KWS 模型，避免身份存储故障时遗留模型资源。
		detector, err := m.wakeWordFactory()
		if err != nil {
			return nil, 0, fmt.Errorf("initialize device wake-word detector: %w", err)
		}
		session = newSession(deviceID, userID, sessionQueueSize)
		session.AudioRouter = audio.NewRouter(detector, m.wakeWordLookback)
		session.LinxClientID = clientID
		m.clientIDs[deviceID] = clientID
		m.sessions[deviceID] = session
		m.logger.Info("device session created", "device_id", deviceID, "session_id", session.ID, "wake_word_lookback_frames", m.wakeWordLookback)
		if m.onCreate != nil {
			m.onCreate(session)
		}
	}

	session.mu.Lock()
	// generation 用来屏蔽旧连接迟到的断开回调，避免误删刚恢复的新会话。
	resuming := session.conn == nil && session.timer != nil
	if session.timer != nil {
		session.timer.Stop()
		session.timer = nil
	}
	if resuming {
		clearQueue(session.AudioIn)
		clearQueue(session.JanusAudioIn)
		clearQueue(session.AudioOut)
		clearQueue(session.ControlIn)
		clearQueue(session.ControlOut)
		clearQueue(session.LinxAudioIn)
		clearQueue(session.LinxEventsIn)
		clearQueue(session.LinxOut)
		m.logger.Info("device session resumed; stale queues cleared", "device_id", deviceID, "session_id", session.ID)
	}
	if session.conn != nil && session.conn != conn {
		_ = session.conn.CloseNow()
	}
	session.generation++
	session.conn = conn
	generation := session.generation
	session.mu.Unlock()
	m.logger.Info("device session attached", "device_id", deviceID, "session_id", session.ID, "generation", generation)
	return session, generation, nil
}

func (m *SessionManager) IsUserOnline(userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		session.mu.Lock()
		connected := session.conn != nil
		session.mu.Unlock()
		if session.UserID == userID && connected {
			return true
		}
	}
	return false
}

func clearQueue[T any](queue chan T) {
	for {
		select {
		case <-queue:
		default:
			return
		}
	}
}

func (m *SessionManager) Disconnect(deviceID string, generation uint64) {
	m.mu.Lock()
	session := m.sessions[deviceID]
	if session == nil {
		m.mu.Unlock()
		return
	}
	session.mu.Lock()
	if session.generation != generation {
		session.mu.Unlock()
		m.mu.Unlock()
		return
	}
	session.conn = nil
	if session.timer != nil {
		session.timer.Stop()
	}
	session.timer = time.AfterFunc(m.grace, func() {
		m.expire(deviceID, session, generation)
	})
	sessionID := session.ID
	session.mu.Unlock()
	m.mu.Unlock()
	m.logger.Info("device session disconnected", "device_id", deviceID, "session_id", sessionID, "grace_seconds", m.grace.Seconds())
}

func (m *SessionManager) expire(deviceID string, expected *Session, generation uint64) {
	m.mu.Lock()
	session := m.sessions[deviceID]
	if session != expected {
		m.mu.Unlock()
		return
	}
	session.mu.Lock()
	// Attach 与超时回调共用管理器锁；过期回调只有在仍是同一代断线会话时才释放资源。
	if session.generation != generation || session.conn != nil {
		session.mu.Unlock()
		m.mu.Unlock()
		return
	}
	delete(m.sessions, deviceID)
	session.mu.Unlock()
	m.mu.Unlock()
	session.cancel()
	LeaveMedia(session)
	session.AudioRouter.Close()
	m.logger.Info("device session expired", "device_id", deviceID, "session_id", session.ID)
}

func (m *SessionManager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *SessionManager) Current(deviceID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[deviceID]
}

func (m *SessionManager) OnlineSession(userID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.UserID != userID {
			continue
		}
		session.mu.Lock()
		connected := session.conn != nil
		session.mu.Unlock()
		if connected {
			return session
		}
	}
	return nil
}

func (m *SessionManager) closeConnection(deviceID string, generation uint64) {
	m.mu.Lock()
	session := m.sessions[deviceID]
	if session != nil {
		session.mu.Lock()
		if session.generation == generation && session.conn != nil {
			_ = session.conn.CloseNow()
			session.conn = nil
		}
		session.mu.Unlock()
	}
	m.mu.Unlock()
}
