package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"

	"reonavr/internal/testconfig"
)

func TestStorePersistsCameraAndCredentialCiphertext(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reonavr.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	camera := domain.Camera{
		ID: "camera-one", Name: "Front Drive", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true,
		ArchiveStream: domain.StreamMain, EventTypes: []domain.EventType{domain.EventMotion, domain.EventVehicle},
		Model: "RLC-810A", Firmware: "test", ConnectionStatus: "Online",
		Capabilities:    &domain.CameraCapabilities{MainStream: "3840x2160", SupportedEventTypes: []domain.EventType{domain.EventMotion, domain.EventVehicle}},
		LastTestedAtUtc: &now, CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("opaque-ciphertext"), Nonce: []byte("opaque-nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.Capabilities.SupportedEventTypes); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListCameras(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "Front Drive" || !items[0].CredentialConfigured {
		t.Fatalf("unexpected cameras: %#v", items)
	}
	storedCredential, err := store.GetCredential(ctx, camera.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedCredential.Ciphertext, credential.Ciphertext) {
		t.Fatal("credential ciphertext was not preserved")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	databaseBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(databaseBytes, []byte("clear-text-password")) {
		t.Fatal("database contains clear-text password")
	}
}

func TestRecoveryIntentAndCoverageSurviveRestartAndPause(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	camera := domain.Camera{ID: "recovery-camera", Name: "Recovery", Host: "camera", Port: 9000, Timezone: "UTC", Enabled: true, ArchiveEnabled: true, RecoveryEnabled: true, ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeContinuous, CreatedAtUtc: now, UpdatedAtUtc: now}
	if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCaptureCoverage(ctx, camera.ID, camera.ArchiveStream, []domain.TimeRange{{StartAtUtc: now, EndAtUtc: now.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SyncRecordingIntent(ctx, camera, true, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	periods, err := store.ListRecoveryPeriods(ctx, camera.ID, now.Add(10*time.Minute))
	if err != nil || len(periods) != 1 || !periods[0].StartAtUtc.Equal(now) {
		t.Fatalf("intent lost across restart: %#v %v", periods, err)
	}
	if err := store.SetRecordingControl(ctx, false, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, now.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	periods, err = store.ListRecoveryPeriods(ctx, camera.ID, now.Add(10*time.Minute))
	if err != nil || len(periods) != 1 || periods[0].EndAtUtc == nil || !periods[0].EndAtUtc.Equal(now.Add(6*time.Minute)) {
		t.Fatalf("pause boundary missing: %#v %v", periods, err)
	}
	if err := store.SetRecordingControl(ctx, true, now.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, now.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	periods, err = store.ListRecoveryPeriods(ctx, camera.ID, now.Add(10*time.Minute))
	if err != nil || len(periods) != 2 || !periods[1].StartAtUtc.Equal(now.Add(8*time.Minute)) {
		t.Fatalf("stale intent included deliberately paused footage: %#v %v", periods, err)
	}
	coverage, err := store.ListVerifiedCoverage(ctx, camera.ID, camera.ArchiveStream, domain.TimeRange{StartAtUtc: now, EndAtUtc: now.Add(10 * time.Minute)}, true)
	if err != nil || len(coverage) != 1 {
		t.Fatalf("coverage not persisted: %#v %v", coverage, err)
	}
}

func TestRecoveryHistoryCoalescesQuietIntervalsAndRetainsUnresolved(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "recovery-history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	start := now.AddDate(0, 0, -40)
	camera := domain.Camera{ID: "history-camera", Name: "History", Host: testconfig.Host(1), Port: 9000, Timezone: "UTC", Enabled: true,
		ArchiveEnabled: true, RecoveryEnabled: true, ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceRtsp,
		RecordingMode: domain.RecordingModeEvents, CreatedAtUtc: start, UpdatedAtUtc: start}
	if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, start); err != nil {
		t.Fatal(err)
	}
	periods, err := store.ListRecoveryPeriods(ctx, camera.ID, now)
	if err != nil || len(periods) != 1 {
		t.Fatalf("missing history intent: %v", err)
	}
	periodID := periods[0].ID
	gaps := []domain.RecoveryGap{
		{ID: "unresolved", TimeRange: domain.TimeRange{StartAtUtc: start, EndAtUtc: start.Add(time.Minute)}},
		{ID: "old-terminal", TimeRange: domain.TimeRange{StartAtUtc: start.Add(time.Minute), EndAtUtc: start.Add(2 * time.Minute)}},
	}
	if err := store.SaveRecoveryScan(ctx, periodID, start.Add(2*time.Minute), gaps); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRecoveryGap(ctx, "old-terminal", "Recovered", 0, start.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for index := range 120 {
		from := now.Add(time.Duration(index-120) * time.Minute)
		gap := domain.RecoveryGap{ID: fmt.Sprintf("quiet-%d", index), TimeRange: domain.TimeRange{StartAtUtc: from, EndAtUtc: from.Add(time.Minute)}}
		if err := store.SaveRecoveryScan(ctx, periodID, gap.EndAtUtc, []domain.RecoveryGap{gap}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateRecoveryGap(ctx, gap.ID, "CheckedEvents", 0, now); err != nil {
			t.Fatal(err)
		}
	}
	history, err := store.ListRecoveryHistory(ctx, camera.ID, 100)
	if err != nil || len(history) != 2 || history[0].ID != "unresolved" || history[1].Status != "CheckedEvents" {
		t.Fatalf("quiet growth hid unresolved history or retained aged terminal rows: %#v %v", history, err)
	}
	if !history[1].StartAtUtc.Equal(now.Add(-2*time.Hour)) || !history[1].EndAtUtc.Equal(now) {
		t.Fatalf("quiet intervals were not coalesced: %#v", history[1])
	}
	recording := domain.Recording{ID: "linked-media", CameraID: camera.ID, NativeName: "linked.mkv", Source: domain.RecordingSourceRtsp,
		Stream: domain.StreamMain, State: domain.RecordingComplete, StartAtUtc: now.Add(-time.Minute), EndAtUtc: now,
		DiscoveredAtUtc: now, UpdatedAtUtc: now}
	if err := store.SaveRtspRecording(ctx, recording); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO RecoveryGapRecordings (GapId, RecordingId) VALUES (?, ?)`, history[1].ID, recording.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRecoveryGap(ctx, history[1].ID, "CheckedEvents", 0, now); err != nil {
		t.Fatalf("linked history must remain valid without coalescing: %v", err)
	}
}

func TestRetentionPreservesUnfinishedRecoveryAndRechecksLegacySuppression(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "retained-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	camera := domain.Camera{ID: "camera-one", Name: "Recovery", Host: "camera", Port: 9000, Enabled: true, ArchiveEnabled: true,
		RecoveryEnabled: true, ArchiveStream: domain.StreamMain, RecordingSource: domain.RecordingSourceRtsp,
		RecordingMode: domain.RecordingModeContinuous, CreatedAtUtc: now, UpdatedAtUtc: now}
	if err := store.CreateCamera(ctx, camera, domain.EncryptedCredential{Ciphertext: []byte("cipher"), Nonce: []byte("nonce"), KeyVersion: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncRecordingIntent(ctx, camera, true, now); err != nil {
		t.Fatal(err)
	}
	periods, err := store.ListRecoveryPeriods(ctx, camera.ID, now.Add(time.Hour))
	if err != nil || len(periods) != 1 {
		t.Fatalf("intent: %#v %v", periods, err)
	}
	gap := domain.RecoveryGap{ID: "gap-one", PeriodID: periods[0].ID, CameraID: camera.ID,
		TimeRange: domain.TimeRange{StartAtUtc: now, EndAtUtc: now.Add(time.Minute)}}
	if err := store.SaveRecoveryScan(ctx, gap.PeriodID, gap.EndAtUtc, []domain.RecoveryGap{gap}); err != nil {
		t.Fatal(err)
	}
	clips := []domain.Recording{
		{ID: "retained", CameraID: camera.ID, NativeName: "first", Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain,
			StartAtUtc: now, EndAtUtc: now.Add(20 * time.Second), State: domain.RecordingQueued, DiscoveredAtUtc: now, UpdatedAtUtc: now},
		{ID: "pending", CameraID: camera.ID, NativeName: "second", Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain,
			StartAtUtc: now.Add(20 * time.Second), EndAtUtc: now.Add(time.Minute), State: domain.RecordingQueued, DiscoveredAtUtc: now, UpdatedAtUtc: now},
	}
	if err := store.QueueRecoveryRecordings(ctx, gap, clips, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRecording(ctx, "retained", "first.mp4", "hash", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecordingCoverage(ctx, "retained", []domain.TimeRange{{StartAtUtc: now, EndAtUtc: now.Add(20 * time.Second)}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRecoveryGap(ctx, gap.ID, "Downloading", 40, now); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRecordingDeleted(ctx, "retained", now); err != nil {
		t.Fatal(err)
	}
	gaps, err := store.ListRecoveryGaps(ctx, camera.ID, 10)
	if err != nil || len(gaps) != 1 || gaps[0].MissingSeconds != 40 || gaps[0].Status != "Downloading" {
		t.Fatalf("retention hid unrelated missing time: %#v %v", gaps, err)
	}
	coverage, err := store.ListVerifiedCoverage(ctx, camera.ID, camera.ArchiveStream, gap.TimeRange, false)
	if err != nil || len(coverage) != 1 {
		t.Fatalf("tombstone coverage lost: %#v %v", coverage, err)
	}
	if final, err := store.RecoveryRecordingFinal(ctx, "retained"); err != nil || !final {
		t.Fatalf("deleted clip eligible for reimport: %v", err)
	}
	if err := store.UpdateRecoveryGap(ctx, gap.ID, "Suppressed", 0, now); err != nil {
		t.Fatal(err)
	}
	gaps, err = store.ListRecoveryGaps(ctx, camera.ID, 10)
	if err != nil || len(gaps) != 1 {
		t.Fatalf("legacy suppression not rechecked: %#v %v", gaps, err)
	}
	claimed, err := store.ClaimNextDownload(ctx, camera.ID, now)
	if err != nil || claimed == nil || claimed.ID != "pending" {
		t.Fatalf("other recovery clip blocked: %#v %v", claimed, err)
	}
}

func TestUserSchemaUsesPascalCaseWithoutUnderscores(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identifiers, err := store.UserSchemaIdentifiers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pascalCase := regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	for _, identifier := range identifiers {
		if !pascalCase.MatchString(identifier) {
			t.Errorf("database identifier %q is not PascalCase", identifier)
		}
	}
}

func TestRecordingControlDefaultsEnabledAndPersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recording-control.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	control, err := store.GetRecordingControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !control.Enabled {
		t.Fatal("recording control did not default to enabled")
	}
	updatedAt := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := store.SetRecordingControl(ctx, false, updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	control, err = store.GetRecordingControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control.Enabled || !control.UpdatedAtUtc.Equal(updatedAt) {
		t.Fatalf("unexpected persisted recording control: %#v", control)
	}
}

func TestApplicationSettingsDefaultAndPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "application-settings.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.GetApplicationSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.RecordingFolder != "." || settings.ArchiveLayout != domain.ArchiveLayoutCameraDate {
		t.Fatalf("unexpected default application settings: %#v", settings)
	}
	settings.RecordingFolder = "recordings"
	settings.ArchiveLayout = domain.ArchiveLayoutDateCamera
	settings.UpdatedAtUtc = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if err := store.SetApplicationSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persisted, err := store.GetApplicationSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.RecordingFolder != "recordings" || persisted.ArchiveLayout != domain.ArchiveLayoutDateCamera || !persisted.UpdatedAtUtc.Equal(settings.UpdatedAtUtc) {
		t.Fatalf("unexpected persisted application settings: %#v", persisted)
	}
}

func TestOpenMigratesRecordingPoliciesAndUnstartedQueue(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recording-policy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE SchemaMigrations (Version INTEGER PRIMARY KEY, AppliedAtUtc TEXT NOT NULL)`,
		`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (1, CURRENT_TIMESTAMP), (2, CURRENT_TIMESTAMP), (3, CURRENT_TIMESTAMP)`,
		`CREATE TABLE Cameras (
			Id TEXT PRIMARY KEY, Name TEXT NOT NULL, Host TEXT NOT NULL,
			Port INTEGER NOT NULL DEFAULT 9000, Channel INTEGER NOT NULL DEFAULT 0,
			Timezone TEXT NOT NULL, Enabled INTEGER NOT NULL DEFAULT 1,
			ArchiveEnabled INTEGER NOT NULL DEFAULT 1,
			ArchiveStream TEXT NOT NULL CHECK (ArchiveStream IN ('Main', 'Sub')),
			Model TEXT, Firmware TEXT, CapabilitiesJson TEXT,
			ConnectionStatus TEXT NOT NULL DEFAULT 'Offline', LastTestedAtUtc TEXT,
			LastEventAtUtc TEXT, LastSearchWatermarkAtUtc TEXT, LastSuccessAtUtc TEXT,
			CreatedAtUtc TEXT NOT NULL, UpdatedAtUtc TEXT NOT NULL, RetiredAtUtc TEXT
		)`,
		`CREATE TABLE Recordings (
			Id TEXT PRIMARY KEY, CameraId TEXT NOT NULL REFERENCES Cameras(Id),
			NativeName TEXT NOT NULL, Stream TEXT NOT NULL CHECK (Stream IN ('Main', 'Sub')),
			StartAtUtc TEXT NOT NULL, EndAtUtc TEXT NOT NULL, SizeBytes INTEGER NOT NULL,
			State TEXT NOT NULL, RawMetadataJson TEXT NOT NULL, DiscoveredAtUtc TEXT NOT NULL,
			UpdatedAtUtc TEXT NOT NULL, LocalPath TEXT, Etag TEXT, Sha256 TEXT,
			CompletedAtUtc TEXT, DeletedAtUtc TEXT, UNIQUE(CameraId, NativeName)
		)`,
		testconfig.Expand("INSERT INTO Cameras (\n\t\t\tId, Name, Host, Timezone, ArchiveStream, CreatedAtUtc, UpdatedAtUtc\n\t\t) VALUES ('camera-one', 'Front', '{{host2}}', 'UTC', 'Main',\n\t\t\t'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')"),
		`INSERT INTO Recordings (
			Id, CameraId, NativeName, Stream, StartAtUtc, EndAtUtc, SizeBytes,
			State, RawMetadataJson, DiscoveredAtUtc, UpdatedAtUtc
		) VALUES
			('queued', 'camera-one', 'queued.mp4', 'Main', '2026-01-01T00:00:00Z',
			 '2026-01-01T00:01:00Z', 1, 'Queued', '{}', '2026-01-01T00:01:00Z', '2026-01-01T00:01:00Z'),
			('complete', 'camera-one', 'complete.mp4', 'Main', '2026-01-01T00:00:00Z',
			 '2026-01-01T00:01:00Z', 1, 'Complete', '{}', '2026-01-01T00:01:00Z', '2026-01-01T00:01:00Z')`,
	}
	for _, statement := range statements {
		if _, err := legacy.ExecContext(ctx, statement); err != nil {
			legacy.Close()
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var source, mode, historicalImport string
	var archiveFrom sql.NullString
	var recoveryEnabled bool
	if err := store.db.QueryRowContext(ctx, `SELECT RecordingSource, RecordingMode, HistoricalImport, ArchiveFromUtc, RecoveryEnabled FROM Cameras WHERE Id = 'camera-one'`).Scan(
		&source, &mode, &historicalImport, &archiveFrom, &recoveryEnabled); err != nil {
		t.Fatal(err)
	}
	if source != "Rtsp" || mode != "Events" || historicalImport != "NewOnly" || !archiveFrom.Valid || !recoveryEnabled {
		t.Fatalf("unexpected migrated policy: %s %s %s", source, mode, historicalImport)
	}
	var queuedState, completeState string
	if err := store.db.QueryRowContext(ctx, `SELECT State FROM Recordings WHERE Id = 'queued'`).Scan(&queuedState); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT State FROM Recordings WHERE Id = 'complete'`).Scan(&completeState); err != nil {
		t.Fatal(err)
	}
	if queuedState != "Discovered" || completeState != "Complete" {
		t.Fatalf("unexpected migrated states: queued=%s complete=%s", queuedState, completeState)
	}
}

func TestOpenMigratesCameraStorageMetadataToAutomaticImport(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "automatic-camera-storage.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000, Timezone: "UTC",
		Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents,
		HistoricalImport: domain.HistoricalImportNewOnly, EventTypes: []domain.EventType{domain.EventPerson},
		Capabilities: &domain.CameraCapabilities{SupportedEventTypes: []domain.EventType{domain.EventPerson, domain.EventVehicle}},
		CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.Capabilities.SupportedEventTypes); err != nil {
		t.Fatal(err)
	}
	recordings := []domain.Recording{
		{ID: "matching", CameraID: camera.ID, NativeName: "person.mp4", Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain, StartAtUtc: now.Add(-time.Hour), EndAtUtc: now.Add(-59 * time.Minute), State: domain.RecordingDiscovered, EventTypes: []domain.EventType{domain.EventPerson}, RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: now, UpdatedAtUtc: now},
		{ID: "excluded", CameraID: camera.ID, NativeName: "vehicle.mp4", Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain, StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, State: domain.RecordingDiscovered, EventTypes: []domain.EventType{domain.EventVehicle}, RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: now, UpdatedAtUtc: now},
	}
	completed := now
	run := domain.IngestionRun{ID: "run-one", CameraID: camera.ID, Kind: "Startup", WindowStartUtc: now.Add(-time.Hour), WindowEndUtc: now, Status: "Complete", DiscoveredCount: len(recordings), StartedAtUtc: now, CompletedAtUtc: &completed}
	if err := store.SaveReconciliation(ctx, run, recordings, now); err != nil {
		t.Fatal(err)
	}
	boundary := now.Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := store.db.ExecContext(ctx, `UPDATE Cameras SET HistoricalImport = 'NewOnly', ArchiveFromUtc = ?, LastSearchWatermarkAtUtc = ? WHERE Id = ?`, boundary, boundary, camera.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM SchemaMigrations WHERE Version = 6`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var historicalImport string
	var archiveFrom, watermark sql.NullString
	if err := store.db.QueryRowContext(ctx, `SELECT HistoricalImport, ArchiveFromUtc, LastSearchWatermarkAtUtc FROM Cameras WHERE Id = ?`, camera.ID).Scan(&historicalImport, &archiveFrom, &watermark); err != nil {
		t.Fatal(err)
	}
	if historicalImport != "AllAvailable" || archiveFrom.Valid || watermark.Valid {
		t.Fatalf("camera-storage policy was not migrated: %s %v %v", historicalImport, archiveFrom, watermark)
	}
	var matchingState, excludedState string
	if err := store.db.QueryRowContext(ctx, `SELECT State FROM Recordings WHERE Id = 'matching'`).Scan(&matchingState); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT State FROM Recordings WHERE Id = 'excluded'`).Scan(&excludedState); err != nil {
		t.Fatal(err)
	}
	if matchingState != "Queued" || excludedState != "Discovered" {
		t.Fatalf("unexpected migrated recording states: matching=%s excluded=%s", matchingState, excludedState)
	}
}

func TestOpenMigratesCameraUniquenessToActiveRegistrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE SchemaMigrations (Version INTEGER PRIMARY KEY, AppliedAtUtc TEXT NOT NULL)`,
		`CREATE TABLE Cameras (
			Id TEXT PRIMARY KEY,
			Name TEXT NOT NULL UNIQUE,
			Host TEXT NOT NULL,
			Port INTEGER NOT NULL DEFAULT 9000,
			Channel INTEGER NOT NULL DEFAULT 0,
			Timezone TEXT NOT NULL,
			Enabled INTEGER NOT NULL DEFAULT 1,
			ArchiveEnabled INTEGER NOT NULL DEFAULT 1,
			ArchiveStream TEXT NOT NULL CHECK (ArchiveStream IN ('Main', 'Sub')),
			Model TEXT,
			Firmware TEXT,
			CapabilitiesJson TEXT,
			ConnectionStatus TEXT NOT NULL DEFAULT 'Offline',
			LastTestedAtUtc TEXT,
			LastEventAtUtc TEXT,
			LastSearchWatermarkAtUtc TEXT,
			LastSuccessAtUtc TEXT,
			CreatedAtUtc TEXT NOT NULL,
			UpdatedAtUtc TEXT NOT NULL,
			RetiredAtUtc TEXT,
			UNIQUE(Host, Channel)
		)`,
		`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (1, CURRENT_TIMESTAMP)`,
		testconfig.Expand("INSERT INTO Cameras (\n\t\t\tId, Name, Host, Port, Channel, Timezone, Enabled, ArchiveEnabled,\n\t\t\tArchiveStream, ConnectionStatus, CreatedAtUtc, UpdatedAtUtc, RetiredAtUtc\n\t\t) VALUES (\n\t\t\t'retired-camera', 'East', '{{host3}}', 9000, 0, 'UTC', 0, 1,\n\t\t\t'Main', 'Offline', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'\n\t\t)"),
	}
	for _, statement := range statements {
		if _, err := legacy.ExecContext(ctx, statement); err != nil {
			legacy.Close()
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	replacement := domain.Camera{
		ID: "replacement-camera", Name: "East", Host: testconfig.Host(3), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true,
		ArchiveStream: domain.StreamMain, EventTypes: []domain.EventType{domain.EventMotion},
		ConnectionStatus: "Online", Capabilities: &domain.CameraCapabilities{SupportedEventTypes: []domain.EventType{domain.EventMotion}},
		LastTestedAtUtc: &now, CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, replacement, credential, replacement.EventTypes); err != nil {
		t.Fatalf("retired registration keys were not released: %v", err)
	}
	var cameraCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM Cameras`).Scan(&cameraCount); err != nil {
		t.Fatal(err)
	}
	if cameraCount != 2 {
		t.Fatalf("retired camera ownership was not preserved: %d rows", cameraCount)
	}
	duplicate := replacement
	duplicate.ID = "duplicate-active-camera"
	if err := store.CreateCamera(ctx, duplicate, credential, duplicate.EventTypes); err == nil {
		t.Fatal("active duplicate registration was accepted")
	}
}

func TestSaveReconciliationIsIdempotentAndAdvancesWatermark(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true,
		ArchiveStream: domain.StreamMain, EventTypes: []domain.EventType{domain.EventMotion},
		ConnectionStatus: "Online", Capabilities: &domain.CameraCapabilities{SupportedEventTypes: []domain.EventType{domain.EventMotion}},
		CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	recording := domain.Recording{
		ID: "recording-one", CameraID: camera.ID,
		NativeName: "Mp4Record/2026-09-11/clip.mp4", Stream: domain.StreamMain,
		StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, SizeBytes: 1024,
		EventTypes: []domain.EventType{domain.EventMotion}, State: domain.RecordingDiscovered,
		RawMetadata: json.RawMessage(`{"name":"clip.mp4"}`), DiscoveredAtUtc: now, UpdatedAtUtc: now,
	}
	completed := now
	firstRun := domain.IngestionRun{
		ID: "run-one", CameraID: camera.ID, Kind: "Startup", WindowStartUtc: now.Add(-48 * time.Hour),
		WindowEndUtc: now, Status: "Complete", DiscoveredCount: 1, StartedAtUtc: now, CompletedAtUtc: &completed,
	}
	if err := store.SaveReconciliation(ctx, firstRun, []domain.Recording{recording}, now); err != nil {
		t.Fatal(err)
	}
	recording.SizeBytes = 2048
	recording.EventTypes = []domain.EventType{domain.EventMotion, domain.EventVehicle}
	recording.State = domain.RecordingQueued
	secondRun := firstRun
	secondRun.ID = "run-two"
	if err := store.SaveReconciliation(ctx, secondRun, []domain.Recording{recording}, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListRecordings(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SizeBytes != 2048 || len(items[0].EventTypes) != 2 || items[0].State != domain.RecordingQueued {
		t.Fatalf("unexpected idempotent recording result: %#v", items)
	}
	watermark, err := store.GetSearchWatermark(ctx, camera.ID)
	if err != nil {
		t.Fatal(err)
	}
	if watermark == nil || !watermark.Equal(now) {
		t.Fatalf("unexpected watermark: %v", watermark)
	}
	var runCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM IngestionRuns`).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 2 {
		t.Fatalf("expected two ingestion runs, got %d", runCount)
	}
}

func TestUpdateCameraPolicyChangeResetsSearchWatermark(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "policy-search.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000, Timezone: "UTC",
		Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents,
		HistoricalImport: domain.HistoricalImportNewOnly, EventTypes: []domain.EventType{domain.EventMotion},
		Capabilities: &domain.CameraCapabilities{SupportedEventTypes: []domain.EventType{domain.EventMotion, domain.EventPerson}},
		CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.Capabilities.SupportedEventTypes); err != nil {
		t.Fatal(err)
	}
	completed := now
	run := domain.IngestionRun{ID: "run-one", CameraID: camera.ID, Kind: "Startup", WindowStartUtc: now.Add(-time.Hour), WindowEndUtc: now, Status: "Complete", StartedAtUtc: now, CompletedAtUtc: &completed}
	if err := store.SaveReconciliation(ctx, run, nil, now); err != nil {
		t.Fatal(err)
	}
	backfill := domain.IngestionRun{ID: "backfill-one", CameraID: camera.ID, Kind: "Backfill", WindowStartUtc: now.Add(-72 * time.Hour), WindowEndUtc: now.Add(-48 * time.Hour), Status: "Complete", DiscoveredCount: 3, StartedAtUtc: now, CompletedAtUtc: &completed}
	if err := store.SaveReconciliation(ctx, backfill, nil, now); err != nil {
		t.Fatal(err)
	}
	cursor, discoveredCount, err := store.GetBackfillCursor(ctx, camera.ID)
	if err != nil || cursor == nil || !cursor.Equal(backfill.WindowStartUtc) || discoveredCount != 3 {
		t.Fatalf("unexpected backfill cursor: %v %d %v", cursor, discoveredCount, err)
	}
	camera.Name = "Front renamed"
	camera.UpdatedAtUtc = now.Add(time.Minute)
	if err := store.UpdateCamera(ctx, camera, nil, camera.Capabilities.SupportedEventTypes); err != nil {
		t.Fatal(err)
	}
	watermark, err := store.GetSearchWatermark(ctx, camera.ID)
	if err != nil || watermark == nil || !watermark.Equal(now) {
		t.Fatalf("non-policy update reset watermark: %v %v", watermark, err)
	}
	camera.EventTypes = []domain.EventType{domain.EventPerson}
	camera.UpdatedAtUtc = now.Add(2 * time.Minute)
	if err := store.UpdateCamera(ctx, camera, nil, camera.Capabilities.SupportedEventTypes); err != nil {
		t.Fatal(err)
	}
	watermark, err = store.GetSearchWatermark(ctx, camera.ID)
	if err != nil || watermark != nil {
		t.Fatalf("policy update did not reset watermark: %v %v", watermark, err)
	}
	cursor, _, err = store.GetBackfillCursor(ctx, camera.ID)
	if err != nil || cursor != nil {
		t.Fatalf("policy update did not reset backfill cursor: %v %v", cursor, err)
	}
}

func TestRecordingDownloadLifecycleIsDurable(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "download.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		EventTypes: []domain.EventType{domain.EventPerson}, CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	recording := domain.Recording{
		ID: "recording-one", CameraID: camera.ID, NativeName: "clip.mp4",
		Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain,
		StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, SizeBytes: 12,
		EventTypes: []domain.EventType{domain.EventPerson}, State: domain.RecordingQueued,
		RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: now, UpdatedAtUtc: now,
	}
	completed := now
	run := domain.IngestionRun{ID: "run-one", CameraID: camera.ID, Kind: "Startup", WindowStartUtc: now.Add(-time.Hour), WindowEndUtc: now, Status: "Complete", DiscoveredCount: 1, StartedAtUtc: now, CompletedAtUtc: &completed}
	if err := store.SaveReconciliation(ctx, run, []domain.Recording{recording}, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextDownload(ctx, camera.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.State != domain.RecordingDownloading || len(claimed.EventTypes) != 1 {
		t.Fatalf("unexpected claim: %#v", claimed)
	}
	if second, err := store.ClaimNextDownload(ctx, camera.ID, now.Add(time.Second)); err != nil || second != nil {
		t.Fatalf("recording was claimed twice: %#v %v", second, err)
	}
	if err := store.RecoverRecordingWork(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimNextDownload(ctx, camera.ID, now.Add(3*time.Second))
	if err != nil || claimed == nil {
		t.Fatalf("recovered recording was not claimable: %#v %v", claimed, err)
	}
	if err := store.CompleteRecording(ctx, claimed.ID, "camera-one/clip.mp4", "abc123", 12, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListRecordings(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].State != domain.RecordingComplete || items[0].LocalPath != "camera-one/clip.mp4" || items[0].Sha256 != "abc123" {
		t.Fatalf("unexpected completed recording: %#v", items)
	}
	loaded, err := store.GetRecording(ctx, claimed.ID)
	if err != nil || loaded.ID != claimed.ID || loaded.LocalPath != "camera-one/clip.mp4" || !slices.Equal(loaded.EventTypes, claimed.EventTypes) {
		t.Fatalf("unexpected recording lookup: %#v %v", loaded, err)
	}
}

func TestClaimNextDownloadPrefersNewestRecording(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "download-order.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, nil); err != nil {
		t.Fatal(err)
	}
	recordings := []domain.Recording{
		{ID: "older", CameraID: camera.ID, NativeName: "older.mp4", Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain, StartAtUtc: now.Add(-time.Hour), EndAtUtc: now.Add(-59 * time.Minute), State: domain.RecordingQueued, RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: now, UpdatedAtUtc: now},
		{ID: "newer", CameraID: camera.ID, NativeName: "newer.mp4", Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain, StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, State: domain.RecordingQueued, RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: now, UpdatedAtUtc: now},
	}
	completed := now
	run := domain.IngestionRun{ID: "run-one", CameraID: camera.ID, Kind: "Startup", WindowStartUtc: now.Add(-time.Hour), WindowEndUtc: now, Status: "Complete", DiscoveredCount: len(recordings), StartedAtUtc: now, CompletedAtUtc: &completed}
	if err := store.SaveReconciliation(ctx, run, recordings, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextDownload(ctx, camera.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != "newer" {
		t.Fatalf("expected newest recording first, got %#v", claimed)
	}
}

func TestSaveRtspRecordingIndexesPublishedMediaAndTags(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "rtsp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeContinuous,
		EventTypes: []domain.EventType{domain.EventPerson}, CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	recording := domain.Recording{
		ID: "rtsp-one", CameraID: camera.ID, NativeName: "Rtsp/2026/09/11/120000-rtsp-one.mkv",
		Source: domain.RecordingSourceRtsp, Stream: domain.StreamMain, Codec: "H.265",
		StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, SizeBytes: 4096,
		EventTypes: []domain.EventType{domain.EventPerson}, State: domain.RecordingComplete,
		RawMetadata: json.RawMessage(`{"capture":"rtsp"}`), DiscoveredAtUtc: now, UpdatedAtUtc: now,
		LocalPath: "camera-one/Rtsp/2026/09/11/120000-rtsp-one.mkv", Sha256: "abc123",
	}
	if err := store.SaveRtspRecording(ctx, recording); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListRecordings(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Source != domain.RecordingSourceRtsp || items[0].State != domain.RecordingComplete ||
		items[0].Codec != "H.265" || items[0].Sha256 != "abc123" || !slices.Equal(items[0].EventTypes, recording.EventTypes) {
		t.Fatalf("unexpected RTSP recording: %#v", items)
	}
}

func TestCameraArchiveStatsOnlyCountTheActiveSource(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "source-stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeContinuous,
		EventTypes: []domain.EventType{domain.EventPerson}, CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	completed := now
	run := domain.IngestionRun{
		ID: "run-one", CameraID: camera.ID, Kind: "Startup", WindowStartUtc: now.Add(-time.Hour),
		WindowEndUtc: now, Status: "Complete", DiscoveredCount: 1, StartedAtUtc: now, CompletedAtUtc: &completed,
	}
	stale := domain.Recording{
		ID: "camera-storage-one", CameraID: camera.ID, NativeName: "old.mp4",
		Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain,
		StartAtUtc: now.Add(-time.Minute), EndAtUtc: now, State: domain.RecordingDiscovered,
		RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: now, UpdatedAtUtc: now,
	}
	if err := store.SaveReconciliation(ctx, run, []domain.Recording{stale}, now); err != nil {
		t.Fatal(err)
	}
	current := domain.Recording{
		ID: "rtsp-one", CameraID: camera.ID, NativeName: "Rtsp/current.mkv",
		Source: domain.RecordingSourceRtsp, Stream: domain.StreamMain,
		StartAtUtc: now, EndAtUtc: now.Add(time.Minute), State: domain.RecordingComplete,
		RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: now, UpdatedAtUtc: now,
		LocalPath: "camera-one/Rtsp/current.mkv",
	}
	if err := store.SaveRtspRecording(ctx, current); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetCamera(ctx, camera.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RecordingCount != 1 || loaded.PendingDownloadCount != 0 {
		t.Fatalf("active source stats included stale metadata: %#v", loaded)
	}
}

func TestUpdateCameraProbeReinterpretsDiscoveredMetadataAfterTimezoneChange(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "timezone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 12, 2, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeEvents,
		EventTypes: []domain.EventType{domain.EventVehicle}, CreatedAtUtc: now, UpdatedAtUtc: now,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	completed := now
	run := domain.IngestionRun{
		ID: "run-one", CameraID: camera.ID, Kind: "Startup", WindowStartUtc: now.Add(-time.Hour),
		WindowEndUtc: now, Status: "Complete", DiscoveredCount: 1, StartedAtUtc: now, CompletedAtUtc: &completed,
	}
	recording := domain.Recording{
		ID: "recording-one", CameraID: camera.ID, NativeName: "local-time.mp4",
		Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain,
		StartAtUtc: time.Date(2026, 9, 11, 18, 43, 21, 0, time.UTC), EndAtUtc: time.Date(2026, 9, 11, 18, 47, 28, 0, time.UTC),
		State:           domain.RecordingDiscovered,
		RawMetadata:     json.RawMessage(`{"startTime":"2026-09-11T18:43:21","endTime":"2026-09-11T18:47:28"}`),
		DiscoveredAtUtc: now, UpdatedAtUtc: now,
	}
	if err := store.SaveReconciliation(ctx, run, []domain.Recording{recording}, now); err != nil {
		t.Fatal(err)
	}
	updatedAt := now.Add(time.Minute)
	if err := store.UpdateCameraProbe(ctx, camera.ID, "RLC-810A", "v1", "America/Los_Angeles", domain.CameraCapabilities{}, "Online", updatedAt); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetCamera(ctx, camera.ID)
	if err != nil || loaded.Timezone != "America/Los_Angeles" {
		t.Fatalf("detected timezone was not saved: %#v, %v", loaded, err)
	}
	items, err := store.ListRecordings(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	expected := time.Date(2026, 9, 12, 1, 43, 21, 0, time.UTC)
	if len(items) != 1 || !items[0].StartAtUtc.Equal(expected) {
		t.Fatalf("recording timestamp was not reinterpreted in the new timezone: %#v", items)
	}
	rules, err := json.Marshal(reolink.TimezoneConfiguration{BaseOffsetSeconds: -8 * 3600})
	if err != nil {
		t.Fatal(err)
	}
	custom := "Camera/" + base64.RawURLEncoding.EncodeToString(rules)
	for _, probe := range []bool{false, true} {
		for _, timezone := range []string{custom, "Camera/Unverified"} {
			if err := store.UpdateCameraProbe(ctx, camera.ID, "Fixture camera", "test", "UTC", domain.CameraCapabilities{}, "Online", updatedAt); err != nil {
				t.Fatal(err)
			}
			if probe {
				err = store.UpdateCameraProbe(ctx, camera.ID, "Fixture camera", "test", timezone, domain.CameraCapabilities{}, "Online", updatedAt)
			} else {
				camera.Timezone = timezone
				err = store.UpdateCamera(ctx, camera, nil, camera.EventTypes)
			}
			if err != nil {
				t.Fatalf("camera-clock update blocked: %v", err)
			}
			items, err = store.ListRecordings(ctx, camera.ID, 10)
			expected = recording.StartAtUtc
			if timezone == custom {
				expected = expected.Add(8 * time.Hour)
			}
			if err != nil || len(items) != 1 || !items[0].StartAtUtc.Equal(expected) {
				t.Fatalf("custom/unverified timestamp handling failed: %#v %v", items, err)
			}
		}
	}
}

func TestApplyEventTransitionPersistsAndClosesEventsIdempotently(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		EventTypes: []domain.EventType{domain.EventPerson}, CreatedAtUtc: started, UpdatedAtUtc: started,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	start := domain.EventTransition{OccurredAtUtc: started, EventType: domain.EventPerson, Active: true, RawPayload: json.RawMessage(`{"eventType":"people"}`)}
	if err := store.ApplyEventTransition(ctx, camera.ID, start); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEventTransition(ctx, camera.ID, start); err != nil {
		t.Fatal(err)
	}
	ended := started.Add(12 * time.Second)
	if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: ended}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListEvents(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != domain.EventPerson || events[0].EndedAtUtc == nil || !events[0].EndedAtUtc.Equal(ended) {
		t.Fatalf("unexpected persisted events: %#v", events)
	}
	if err := store.MarkEventFinalizationMiss(ctx, events[0].ID); err != nil {
		t.Fatal(err)
	}
	ready, err := store.ListEventsReadyForFinalization(ctx, ended.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("event finalized without media remained pending: %#v", ready)
	}
	cameras, err := store.ListCameras(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cameras) != 1 || cameras[0].LastEventAtUtc == nil || !cameras[0].LastEventAtUtc.Equal(ended) {
		t.Fatalf("unexpected camera event time: %#v", cameras)
	}
}

func TestArchivedViewsExposeEventsOnlyAfterTheirMediaIsLocal(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "archived-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	camera := domain.Camera{
		ID: "camera-one", Name: "Front", Host: testconfig.Host(2), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamMain,
		RecordingSource: domain.RecordingSourceCameraStorage, RecordingMode: domain.RecordingModeEvents,
		HistoricalImport: domain.HistoricalImportAll, EventTypes: []domain.EventType{domain.EventVehicle},
		CreatedAtUtc: started, UpdatedAtUtc: started,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: started, EventType: domain.EventVehicle, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: ended}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListEvents(ctx, camera.ID, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("unexpected internal events: %#v %v", events, err)
	}
	recording := domain.Recording{
		ID: "recording-one", CameraID: camera.ID, NativeName: "vehicle.mp4",
		Source: domain.RecordingSourceCameraStorage, Stream: domain.StreamMain,
		StartAtUtc: started.Add(-time.Second), EndAtUtc: ended.Add(time.Second), SizeBytes: 1024,
		EventTypes: []domain.EventType{domain.EventVehicle}, State: domain.RecordingQueued,
		RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: ended, UpdatedAtUtc: ended,
	}
	completed := ended
	run := domain.IngestionRun{
		ID: "event-run", CameraID: camera.ID, Kind: "EventFinalization",
		WindowStartUtc: recording.StartAtUtc, WindowEndUtc: recording.EndAtUtc,
		Status: "Complete", DiscoveredCount: 1, StartedAtUtc: ended, CompletedAtUtc: &completed,
	}
	if err := store.SaveEventFinalization(ctx, events[0].ID, run, []domain.Recording{recording}, recording.NativeName, ended); err != nil {
		t.Fatal(err)
	}
	archivedRecordings, err := store.ListArchivedRecordings(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	archivedEvents, err := store.ListArchivedEvents(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(archivedRecordings) != 0 || len(archivedEvents) != 0 {
		t.Fatalf("pending media leaked into public archive views: recordings=%#v events=%#v", archivedRecordings, archivedEvents)
	}
	if err := store.CompleteRecording(ctx, recording.ID, "camera-one/vehicle.mp4", "abc123", recording.SizeBytes, ended.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecordingCoverage(ctx, recording.ID, []domain.TimeRange{{StartAtUtc: recording.StartAtUtc, EndAtUtc: recording.EndAtUtc}}); err != nil {
		t.Fatal(err)
	}
	archivedRecordings, err = store.ListArchivedRecordings(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	archivedEvents, err = store.ListArchivedEvents(ctx, camera.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(archivedRecordings) != 1 || len(archivedEvents) != 1 || archivedEvents[0].NativeRecordingName != recording.NativeName {
		t.Fatalf("completed media and event were not exposed together: recordings=%#v events=%#v", archivedRecordings, archivedEvents)
	}
}

func TestArchivedEventsIncludeOnlyEventsCoveredByLocalRtspSegments(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "archived-rtsp-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	camera := domain.Camera{
		ID: "camera-one", Name: "Living", Host: testconfig.Host(6), Port: 9000,
		Timezone: "UTC", Enabled: true, ArchiveEnabled: true, ArchiveStream: domain.StreamSub,
		RecordingSource: domain.RecordingSourceRtsp, RecordingMode: domain.RecordingModeContinuous,
		EventTypes: []domain.EventType{domain.EventPerson}, CreatedAtUtc: started, UpdatedAtUtc: started,
	}
	credential := domain.EncryptedCredential{Username: testconfig.Username(), Ciphertext: []byte("ciphertext"), Nonce: []byte("nonce"), KeyVersion: 1}
	if err := store.CreateCamera(ctx, camera, credential, camera.EventTypes); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: started, EventType: domain.EventPerson, Active: true}); err != nil {
		t.Fatal(err)
	}
	ended := started.Add(10 * time.Second)
	if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: ended}); err != nil {
		t.Fatal(err)
	}
	if events, err := store.ListArchivedEvents(ctx, camera.ID, 10); err != nil || len(events) != 0 {
		t.Fatalf("event without local RTSP media was exposed: %#v %v", events, err)
	}
	recording := domain.Recording{
		ID: "rtsp-one", CameraID: camera.ID, NativeName: "Rtsp/segment.mkv",
		Source: domain.RecordingSourceRtsp, Stream: domain.StreamSub,
		StartAtUtc: started.Add(-time.Second), EndAtUtc: started.Add(time.Minute), SizeBytes: 1024,
		EventTypes: []domain.EventType{domain.EventPerson}, State: domain.RecordingComplete,
		RawMetadata: json.RawMessage(`{}`), DiscoveredAtUtc: ended, UpdatedAtUtc: ended,
		LocalPath: "camera-one/Rtsp/segment.mkv", Sha256: "abc123",
	}
	if err := store.SaveRtspRecording(ctx, recording); err != nil {
		t.Fatal(err)
	}
	if events, err := store.ListArchivedEvents(ctx, camera.ID, 10); err != nil || len(events) != 0 {
		t.Fatalf("unverified recording bounds were treated as event coverage: %#v %v", events, err)
	}
	if err := store.SaveRecordingCoverage(ctx, recording.ID, []domain.TimeRange{
		{StartAtUtc: started, EndAtUtc: ended},
		{StartAtUtc: started.Add(50 * time.Second), EndAtUtc: started.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: started.Add(30 * time.Second), EventType: domain.EventPerson, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEventTransition(ctx, camera.ID, domain.EventTransition{OccurredAtUtc: started.Add(35 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListArchivedEvents(ctx, camera.ID, 10)
	if err != nil || len(events) != 1 || events[0].EventType != domain.EventPerson {
		t.Fatalf("event covered by local RTSP media was not exposed: %#v %v", events, err)
	}
	if !events[0].StartedAtUtc.Equal(started) {
		t.Fatalf("event inside verified video hole was exposed: %#v", events)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE Recordings SET Source = 'CameraStorage', NativeName = 'recovered-native.mp4' WHERE Id = ?`, recording.ID); err != nil {
		t.Fatal(err)
	}
	events, err = store.ListArchivedEvents(ctx, camera.ID, 10)
	if err != nil || len(events) != 1 || !events[0].StartedAtUtc.Equal(started) {
		t.Fatalf("verified recovered SD coverage did not back the event: %#v %v", events, err)
	}
}

func TestRecurringOperationalIssueIsReopened(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "issues.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	issue := domain.OperationalIssue{
		ID: "issue-one", Severity: "Error", Source: "RetentionWorker",
		ErrorCode: "WorkerCycleFailed", Message: "capacity check failed",
		FirstSeenAtUtc: now, LastSeenAtUtc: now,
	}
	if err := store.ReportIssue(ctx, issue); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveIssue(ctx, issue.Source, "", issue.ErrorCode, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	issue.LastSeenAtUtc = now.Add(2 * time.Minute)
	if err := store.ReportIssue(ctx, issue); err != nil {
		t.Fatal(err)
	}
	issues, err := store.ListOperationalIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].ResolvedAtUtc != nil || issues[0].OccurrenceCount != 2 {
		t.Fatalf("recurring issue was not reopened: %#v", issues)
	}
}
