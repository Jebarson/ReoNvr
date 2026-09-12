package camera

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
	"reonavr/internal/repository"
	"reonavr/internal/security"
)

type fakeTester struct {
	passwords        []string
	result           reolink.TestResult
	rejectedPassword string
}

func (tester *fakeTester) Test(_ context.Context, connection reolink.Connection) (reolink.TestResult, error) {
	tester.passwords = append(tester.passwords, connection.Password)
	if tester.rejectedPassword != "" && connection.Password == tester.rejectedPassword {
		return reolink.TestResult{}, errors.New("authentication failed")
	}
	return tester.result, nil
}

func TestRegistrationRequiresReceiptAndRetainsCredentialForPolicyEdit(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tester := &fakeTester{result: testCameraResult()}
	service := NewService(store, tester, cipher)
	draft := validDraft()

	if _, err := service.Create(ctx, draft); !errors.Is(err, ErrRetest) {
		t.Fatalf("expected receipt requirement, got %v", err)
	}
	testResult, err := service.Test(ctx, "", draft)
	if err != nil {
		t.Fatal(err)
	}
	draft.TestID = testResult.TestID
	created, err := service.Create(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	if created.Timezone != "America/Los_Angeles" {
		t.Fatalf("camera did not use its detected timezone: %#v", created)
	}
	if created.HistoricalImport != domain.HistoricalImportAll || created.ArchiveFromUtc != nil {
		t.Fatalf("camera storage did not enable automatic historical import: %#v", created)
	}
	credentialBefore, err := store.GetCredential(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err := cipher.Decrypt(created.ID, credentialBefore.Ciphertext, credentialBefore.Nonce)
	if err != nil || password != draft.Password {
		t.Fatalf("stored credential did not decrypt: %v", err)
	}

	draft.TestID = ""
	draft.Password = ""
	draft.Enabled = false
	draft.Name = "Driveway"
	updated, err := service.Update(ctx, created.ID, draft)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || updated.ConnectionStatus != "Offline" {
		t.Fatalf("unexpected policy update: %#v", updated)
	}
	credentialAfter, err := store.GetCredential(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(credentialBefore.Ciphertext, credentialAfter.Ciphertext) || !bytes.Equal(credentialBefore.Nonce, credentialAfter.Nonce) {
		t.Fatal("policy-only edit rewrote the credential")
	}
	draft.Enabled = true
	reenabled, err := service.Update(ctx, created.ID, draft)
	if err != nil {
		t.Fatal(err)
	}
	if !reenabled.Enabled || reenabled.ConnectionStatus != "Testing" {
		t.Fatalf("unexpected re-enable status: %#v", reenabled)
	}
}

func TestConnectionEditCanTestWithStoredPassword(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "edit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tester := &fakeTester{result: testCameraResult()}
	service := NewService(store, tester, cipher)
	draft := validDraft()
	testResult, err := service.Test(ctx, "", draft)
	if err != nil {
		t.Fatal(err)
	}
	draft.TestID = testResult.TestID
	created, err := service.Create(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}

	draft.Password = ""
	draft.TestID = ""
	draft.Host = "10.0.0.153"
	if _, err := service.Update(ctx, created.ID, draft); !errors.Is(err, ErrRetest) {
		t.Fatalf("expected retest for host change, got %v", err)
	}
	testResult, err = service.Test(ctx, created.ID, draft)
	if err != nil {
		t.Fatal(err)
	}
	if tester.passwords[len(tester.passwords)-1] != "camera-password" {
		t.Fatal("stored password was not used for edit test")
	}
	draft.TestID = testResult.TestID
	updated, err := service.Update(ctx, created.ID, draft)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Host != "10.0.0.153" {
		t.Fatalf("host was not updated: %#v", updated)
	}
}

func TestStreamQualityChangesDoNotRequireRetest(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "stream-quality.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeTester{result: testCameraResult()}, cipher)
	draft := validDraft()
	testResult, err := service.Test(ctx, "", draft)
	if err != nil {
		t.Fatal(err)
	}
	draft.ArchiveStream = domain.StreamSub
	draft.TestID = testResult.TestID
	created, err := service.Create(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	if created.ArchiveStream != domain.StreamSub {
		t.Fatalf("stream quality was not saved: %#v", created)
	}

	draft.ArchiveStream = domain.StreamMain
	draft.TestID = ""
	draft.Password = ""
	updated, err := service.Update(ctx, created.ID, draft)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ArchiveStream != domain.StreamMain {
		t.Fatalf("updated stream quality was not saved: %#v", updated)
	}
}

func TestConnectionTestReturnsSnapshotDataURL(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	result := testCameraResult()
	result.SnapshotJPEG = []byte{0xff, 0xd8, 0x01, 0xff, 0xd9}
	service := NewService(store, &fakeTester{result: result}, cipher)

	response, err := service.Test(ctx, "", validDraft())
	if err != nil {
		t.Fatal(err)
	}
	if response.SnapshotDataURL != "data:image/jpeg;base64,/9gB/9k=" || response.TimezoneOffsetSeconds != -28800 {
		t.Fatalf("unexpected connection result: %#v", response)
	}
}

func TestReceiptExpiresAndCanOnlyBeUsedOnce(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "receipt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, _ := security.NewCredentialCipher(bytes.Repeat([]byte{5}, 32))
	service := NewService(store, &fakeTester{result: testCameraResult()}, cipher)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	service.now = func() time.Time { return now }
	draft := validDraft()
	response, err := service.Test(ctx, "", draft)
	if err != nil {
		t.Fatal(err)
	}
	draft.TestID = response.TestID
	service.now = func() time.Time { return now.Add(6 * time.Minute) }
	if _, err := service.Create(ctx, draft); !errors.Is(err, ErrRetest) {
		t.Fatalf("expected expired receipt rejection, got %v", err)
	}
}

func TestRegistrationAllowsEmptyPassword(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "empty-password.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tester := &fakeTester{result: testCameraResult()}
	service := NewService(store, tester, cipher)
	draft := validDraft()
	draft.Password = ""
	testResult, err := service.Test(ctx, "", draft)
	if err != nil {
		t.Fatal(err)
	}
	draft.TestID = testResult.TestID
	created, err := service.Create(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := store.GetCredential(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err := cipher.Decrypt(created.ID, credential.Ciphertext, credential.Nonce)
	if err != nil || password != "" {
		t.Fatalf("empty password was not preserved: %q, %v", password, err)
	}
}

func validDraft() Draft {
	return Draft{
		Name: "Front Drive", Host: "10.0.0.152", Port: 9000, Channel: 0,
		Timezone: "UTC", Username: "admin", Password: "camera-password",
		Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents,
		HistoricalImport: domain.HistoricalImportNewOnly, PreEventSeconds: 10, PostEventSeconds: 15,
		EventTypes: []domain.EventType{domain.EventMotion, domain.EventPerson},
	}
}

func testCameraResult() reolink.TestResult {
	return reolink.TestResult{
		Model: "RLC-810A", Firmware: "v3.1.0.956", Timezone: "America/Los_Angeles", TimezoneOffsetSeconds: -28800,
		Capabilities: domain.CameraCapabilities{
			MainStream: "3840x2160", SubStream: "640x360", PreRecordSeconds: 10,
			PostRecordSeconds: 15, Storage: "Ready", StorageAvailable: true, RtspAvailable: true,
			SupportedEventTypes: []domain.EventType{domain.EventMotion, domain.EventPerson, domain.EventVehicle, domain.EventAnimal},
		},
	}
}

func TestRecordingPolicyRequiresTestedCapabilities(t *testing.T) {
	capabilities := testCameraResult().Capabilities
	draft := validDraft()
	capabilities.StorageAvailable = false
	if err := validateRecordingPolicy(draft, capabilities); err == nil {
		t.Fatal("camera-storage source was accepted without ready storage")
	}
	capabilities.StorageAvailable = true
	draft.RecordingMode = domain.RecordingModeContinuous
	if err := validateRecordingPolicy(draft, capabilities); err == nil {
		t.Fatal("camera-storage continuous mode was accepted without a continuous schedule")
	}
	draft.RecordingSource = domain.RecordingSourceRtsp
	capabilities.RtspAvailable = false
	if err := validateRecordingPolicy(draft, capabilities); err == nil {
		t.Fatal("RTSP source was accepted when the selected stream is unavailable")
	}
}

func TestConnectionTestUsesSubmittedPasswordInsteadOfStored(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "submitted-password.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{6}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tester := &fakeTester{result: testCameraResult()}
	service := NewService(store, tester, cipher)
	draft := validDraft()
	testResult, err := service.Test(ctx, "", draft)
	if err != nil {
		t.Fatal(err)
	}
	draft.TestID = testResult.TestID
	created, err := service.Create(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}

	tester.rejectedPassword = "wrong-password"
	draft.Password = tester.rejectedPassword
	if _, err := service.Test(ctx, created.ID, draft); err == nil {
		t.Fatal("expected submitted password to fail authentication")
	}
	if tester.passwords[len(tester.passwords)-1] != tester.rejectedPassword {
		t.Fatal("connection test did not use the submitted password")
	}
}
