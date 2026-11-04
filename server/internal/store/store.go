package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrPairCodeInvalid = errors.New("pair code is invalid or expired")
var ErrNotFound = errors.New("record not found")

type Store struct {
	db   *sql.DB
	path string
}

type Room struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, device_id TEXT UNIQUE NOT NULL);
CREATE TABLE IF NOT EXISTS rooms (id TEXT PRIMARY KEY, name TEXT NOT NULL, owner_id TEXT NOT NULL REFERENCES users(id));
CREATE TABLE IF NOT EXISTS room_members (room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE, user_id TEXT NOT NULL REFERENCES users(id), PRIMARY KEY(room_id, user_id));
CREATE TABLE IF NOT EXISTS pair_codes (code TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES users(id), expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS friendships (user_a TEXT NOT NULL REFERENCES users(id), user_b TEXT NOT NULL REFERENCES users(id), created_at INTEGER NOT NULL, PRIMARY KEY(user_a, user_b), CHECK(user_a < user_b));
CREATE TABLE IF NOT EXISTS invitations (id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE, inviter_id TEXT NOT NULL REFERENCES users(id), invitee_id TEXT NOT NULL REFERENCES users(id), status TEXT NOT NULL, created_at INTEGER NOT NULL);`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize sqlite schema: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) EnsureUser(deviceID string) (string, error) {
	if deviceID == "" {
		return "", errors.New("device ID is required")
	}
	if _, err := s.db.Exec(`INSERT INTO users(id, device_id) VALUES(?, ?) ON CONFLICT(device_id) DO NOTHING`, newID(), deviceID); err != nil {
		return "", fmt.Errorf("ensure device user: %w", err)
	}
	var id string
	err := s.db.QueryRow(`SELECT id FROM users WHERE device_id = ?`, deviceID).Scan(&id)
	return id, err
}

func (s *Store) UserForDevice(deviceID string) (string, error) {
	var userID string
	err := s.db.QueryRow(`SELECT id FROM users WHERE device_id = ?`, deviceID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return userID, err
}

func (s *Store) CreateRoom(ownerID, name string) (string, error) {
	if ownerID == "" || name == "" {
		return "", errors.New("room owner and name are required")
	}
	id := newID()
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO rooms(id, name, owner_id) VALUES(?, ?, ?)`, id, name, ownerID); err != nil {
		return "", fmt.Errorf("create room: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO room_members(room_id, user_id) VALUES(?, ?)`, id, ownerID); err != nil {
		return "", fmt.Errorf("add room owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) GetRoom(roomID string) (Room, error) {
	var room Room
	err := s.db.QueryRow(`SELECT id, name, owner_id FROM rooms WHERE id = ?`, roomID).Scan(&room.ID, &room.Name, &room.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Room{}, ErrNotFound
	}
	return room, err
}

func (s *Store) Invite(roomID, inviterID, inviteeID string, online bool) (string, error) {
	if !online {
		return "", errors.New("invitee is offline")
	}
	if inviterID == inviteeID || !s.AreFriends(inviterID, inviteeID) {
		return "", errors.New("invitee is not a friend")
	}
	var member int
	if err := s.db.QueryRow(`SELECT 1 FROM room_members WHERE room_id = ? AND user_id = ?`, roomID, inviterID).Scan(&member); err != nil {
		return "", errors.New("inviter is not a room member")
	}
	id := newID()
	_, err := s.db.Exec(`INSERT INTO invitations(id, room_id, inviter_id, invitee_id, status, created_at) VALUES(?, ?, ?, ?, 'pending', ?)`, id, roomID, inviterID, inviteeID, time.Now().UnixMilli())
	if err != nil {
		return "", fmt.Errorf("create room invitation: %w", err)
	}
	return id, nil
}

func (s *Store) AcceptInvitation(invitationID, userID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var roomID string
	err = tx.QueryRow(`SELECT room_id FROM invitations WHERE id = ? AND invitee_id = ? AND status = 'pending'`, invitationID, userID).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO room_members(room_id, user_id) VALUES(?, ?)`, roomID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE invitations SET status = 'accepted' WHERE id = ?`, invitationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RoomMembers(roomID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT user_id FROM room_members WHERE room_id = ? ORDER BY user_id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		members = append(members, userID)
	}
	return members, rows.Err()
}

func (s *Store) RoomsForUser(userID string) ([]Room, error) {
	rows, err := s.db.Query(`SELECT r.id, r.name, r.owner_id FROM rooms r JOIN room_members m ON m.room_id = r.id WHERE m.user_id = ? ORDER BY r.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []Room
	for rows.Next() {
		var room Room
		if err := rows.Scan(&room.ID, &room.Name, &room.OwnerID); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

func (s *Store) CreatePairCode(ownerID string, ttl time.Duration) (string, error) {
	if ownerID == "" || ttl <= 0 {
		return "", errors.New("pair code owner and positive ttl are required")
	}
	var bytes [4]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate pair code: %w", err)
	}
	code := fmt.Sprintf("%06d", uint32(bytes[0])<<24|uint32(bytes[1])<<16|uint32(bytes[2])<<8|uint32(bytes[3])%1000000)
	code = code[len(code)-6:]
	_, err := s.db.Exec(`INSERT INTO pair_codes(code, owner_id, expires_at) VALUES(?, ?, ?)`, code, ownerID, time.Now().Add(ttl).UnixMilli())
	if err != nil {
		return "", fmt.Errorf("store pair code: %w", err)
	}
	return code, nil
}

func (s *Store) AcceptPairCode(accepterID, code string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ownerID string
	err = tx.QueryRow(`SELECT owner_id FROM pair_codes WHERE code = ? AND expires_at > ?`, code, time.Now().UnixMilli()).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ownerID == accepterID) {
		return ErrPairCodeInvalid
	}
	if err != nil {
		return err
	}
	a, b := ownerID, accepterID
	if b < a {
		a, b = b, a
	}
	if _, err := tx.Exec(`INSERT INTO friendships(user_a, user_b, created_at) VALUES(?, ?, ?)`, a, b, time.Now().Unix()); err != nil {
		return fmt.Errorf("create friendship: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM pair_codes WHERE code = ?`, code); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AreFriends(userA, userB string) bool {
	if userB < userA {
		userA, userB = userB, userA
	}
	var exists int
	return s.db.QueryRow(`SELECT 1 FROM friendships WHERE user_a = ? AND user_b = ?`, userA, userB).Scan(&exists) == nil
}

func newID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(fmt.Sprintf("generate id: %v", err))
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}
