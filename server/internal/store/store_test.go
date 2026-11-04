package store

import (
	"errors"
	"testing"
	"time"
)

func TestDeviceIdentityAndRoomPersist(t *testing.T) {
	db := openTestStore(t)
	userID, err := db.EnsureUser("device-a")
	if err != nil {
		t.Fatal(err)
	}
	roomID, err := db.CreateRoom(userID, "Kitchen")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTestStoreAt(t, db.path)
	defer db.Close()
	gotUser, err := db.EnsureUser("device-a")
	if err != nil || gotUser != userID {
		t.Fatalf("user identity = %q, %v; want %q", gotUser, err, userID)
	}
	if _, err := db.GetRoom(roomID); err != nil {
		t.Fatalf("room did not persist: %v", err)
	}
}

func TestPairingRequiresBothUsersAndConsumesOneTimeCode(t *testing.T) {
	db := openTestStore(t)
	defer db.Close()
	a, _ := db.EnsureUser("device-a")
	b, _ := db.EnsureUser("device-b")
	code, err := db.CreatePairCode(a, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := db.AcceptPairCode(b, code)
	if err != nil {
		t.Fatal(err)
	}
	if db.AreFriends(a, b) {
		t.Fatal("pairing code submission created friendship before owner confirmation")
	}
	if err := db.ConfirmFriendship(b, requestID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("requester confirmation error = %v, want ErrNotFound", err)
	}
	if err := db.ConfirmFriendship(a, requestID); err != nil {
		t.Fatal(err)
	}
	if !db.AreFriends(a, b) {
		t.Fatal("friendship missing after both users confirmed pairing")
	}
	if _, err := db.AcceptPairCode(b, code); !errors.Is(err, ErrPairCodeInvalid) {
		t.Fatalf("reused pair code error = %v, want ErrPairCodeInvalid", err)
	}
}

func TestPairingCodeExpires(t *testing.T) {
	db := openTestStore(t)
	defer db.Close()
	a, _ := db.EnsureUser("device-a")
	b, _ := db.EnsureUser("device-b")
	code, err := db.CreatePairCode(a, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := db.AcceptPairCode(b, code); !errors.Is(err, ErrPairCodeInvalid) {
		t.Fatalf("expired code error = %v, want ErrPairCodeInvalid", err)
	}
}

func TestRoomInviteRequiresOnlineFriendAndInviteeAcceptance(t *testing.T) {
	db := openTestStore(t)
	defer db.Close()
	owner, _ := db.EnsureUser("device-owner")
	friend, _ := db.EnsureUser("device-friend")
	code, err := db.CreatePairCode(owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := db.AcceptPairCode(friend, code)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ConfirmFriendship(owner, requestID); err != nil {
		t.Fatal(err)
	}
	roomID, err := db.CreateRoom(owner, "Workshop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Invite(roomID, owner, friend, false); err == nil {
		t.Fatal("offline invite should fail immediately")
	}
	invitationID, err := db.Invite(roomID, owner, friend, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AcceptInvitation(invitationID, friend); err != nil {
		t.Fatal(err)
	}
	members, err := db.RoomMembers(roomID)
	if err != nil || len(members) != 2 {
		t.Fatalf("room members = %v, %v; want owner and accepted invitee", members, err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStoreAt(t, t.TempDir()+"/intercom.db")
}

func openTestStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
