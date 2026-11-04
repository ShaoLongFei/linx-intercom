package device

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"linx-intercom/server/internal/audio"
)

const sessionQueueSize = 32

type Session struct {
	ID       string
	DeviceID string
	UserID   string

	AudioIn       chan []byte
	JanusAudioIn  chan []byte
	AudioOut      chan []byte
	ControlIn     chan ControlMessage
	ControlOut    chan ControlMessage
	LinxAudioIn   chan []byte
	LinxAudioOut  chan []byte
	LinxEventsIn  chan json.RawMessage
	LinxEventsOut chan json.RawMessage
	AudioRouter   *audio.Router
	Done          <-chan struct{}

	mu          sync.Mutex
	conn        *websocket.Conn
	generation  uint64
	timer       *time.Timer
	cancel      context.CancelFunc
	mediaJoinMu sync.Mutex
	mediaMu     sync.Mutex
	media       *roomMediaBinding
}

func newSession(deviceID, userID string, queueSize int) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		ID:            newID(),
		DeviceID:      deviceID,
		UserID:        userID,
		AudioIn:       make(chan []byte, queueSize),
		JanusAudioIn:  make(chan []byte, queueSize),
		AudioOut:      make(chan []byte, queueSize),
		ControlIn:     make(chan ControlMessage, queueSize),
		ControlOut:    make(chan ControlMessage, queueSize),
		LinxAudioIn:   make(chan []byte, queueSize),
		LinxAudioOut:  make(chan []byte, queueSize),
		LinxEventsIn:  make(chan json.RawMessage, queueSize),
		LinxEventsOut: make(chan json.RawMessage, queueSize),
		AudioRouter:   audio.NewRouter(audio.DisabledDetector{}, 15),
		Done:          ctx.Done(),
		cancel:        cancel,
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
	return enqueueAudio(s.AudioOut, frame)
}

func (s *Session) ConnectionGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

func enqueueAudio(queue chan []byte, frame []byte) bool {
	copyOfFrame := append([]byte(nil), frame...)
	dropped := false
	select {
	case queue <- copyOfFrame:
		return false
	default:
	}
	select {
	case <-queue:
		dropped = true
	default:
	}
	select {
	case queue <- copyOfFrame:
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
	mu          sync.Mutex
	grace       time.Duration
	sessions    map[string]*Session
	userIDs     map[string]string
	logger      *slog.Logger
	resolveUser func(string) (string, error)
	onCreate    func(*Session)
}

func NewSessionManager(grace time.Duration) *SessionManager {
	if grace <= 0 {
		grace = 60 * time.Second
	}
	return &SessionManager{
		grace:    grace,
		sessions: make(map[string]*Session),
		userIDs:  make(map[string]string),
		logger:   slog.Default(),
	}
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
		session = newSession(deviceID, userID, sessionQueueSize)
		m.sessions[deviceID] = session
		m.logger.Info("device session created", "device_id", deviceID, "session_id", session.ID)
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
		clearQueue(session.LinxAudioOut)
		clearQueue(session.LinxEventsIn)
		clearQueue(session.LinxEventsOut)
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
