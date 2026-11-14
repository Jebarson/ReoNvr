package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS SchemaMigrations (
			Version INTEGER PRIMARY KEY,
			AppliedAtUtc TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS Cameras (
			Id TEXT PRIMARY KEY,
			Name TEXT NOT NULL,
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
			RetiredAtUtc TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS CameraCredentials (
			CameraId TEXT PRIMARY KEY REFERENCES Cameras(Id),
			Username TEXT NOT NULL,
			PasswordCiphertext BLOB NOT NULL,
			PasswordNonce BLOB NOT NULL,
			KeyVersion INTEGER NOT NULL,
			UpdatedAtUtc TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS CameraEventTypes (
			CameraId TEXT NOT NULL REFERENCES Cameras(Id),
			EventType TEXT NOT NULL,
			Enabled INTEGER NOT NULL,
			Supported INTEGER NOT NULL,
			PRIMARY KEY(CameraId, EventType)
		)`,
		`CREATE TABLE IF NOT EXISTS WorkerStates (
			WorkerName TEXT NOT NULL,
			ScopeKey TEXT NOT NULL,
			Stage TEXT NOT NULL,
			LastHeartbeatAtUtc TEXT,
			LastSuccessAtUtc TEXT,
			ConsecutiveFailures INTEGER NOT NULL DEFAULT 0,
			NextAttemptAtUtc TEXT,
			LastErrorCode TEXT,
			UpdatedAtUtc TEXT NOT NULL,
			PRIMARY KEY(WorkerName, ScopeKey)
		)`,
		`CREATE TABLE IF NOT EXISTS OperationalIssues (
			Id TEXT PRIMARY KEY,
			Severity TEXT NOT NULL,
			Source TEXT NOT NULL,
			CameraId TEXT REFERENCES Cameras(Id),
			WorkItemId TEXT,
			ErrorCode TEXT NOT NULL,
			Message TEXT NOT NULL,
			FirstSeenAtUtc TEXT NOT NULL,
			LastSeenAtUtc TEXT NOT NULL,
			OccurrenceCount INTEGER NOT NULL DEFAULT 1,
			ResolvedAtUtc TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS IndexOperationalIssuesOpen ON OperationalIssues(Source, IFNULL(CameraId, ''), ErrorCode) WHERE ResolvedAtUtc IS NULL`,
		`INSERT OR IGNORE INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (1, CURRENT_TIMESTAMP)`,
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize database: %w", err)
		}
	}
	if err := store.migrateActiveCameraUniqueness(ctx); err != nil {
		return err
	}
	if err := store.migrateArchiveSchema(ctx); err != nil {
		return err
	}
	if err := store.migrateRecordingPolicies(ctx); err != nil {
		return err
	}
	if err := store.migrateApplicationSettings(ctx); err != nil {
		return err
	}
	if err := store.migrateAutomaticCameraStorageImport(ctx); err != nil {
		return err
	}
	if err := store.migrateStorageSettings(ctx); err != nil {
		return err
	}
	if err := store.migrateRecovery(ctx); err != nil {
		return err
	}
	return store.migrateRecoveryRetries(ctx)
}

func (store *Store) migrateActiveCameraUniqueness(ctx context.Context) error {
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SchemaMigrations WHERE Version = 2`).Scan(&applied); err != nil {
		return fmt.Errorf("check camera uniqueness migration: %w", err)
	}
	if applied != 0 {
		return nil
	}

	connection, err := store.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for migration: %w", err)
	}
	defer connection.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`)

	transaction, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin camera uniqueness migration: %w", err)
	}
	statements := []string{
		`CREATE TABLE CamerasNext (
			Id TEXT PRIMARY KEY,
			Name TEXT NOT NULL,
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
			RetiredAtUtc TEXT
		)`,
		`INSERT INTO CamerasNext (
			Id, Name, Host, Port, Channel, Timezone, Enabled, ArchiveEnabled,
			ArchiveStream, Model, Firmware, CapabilitiesJson, ConnectionStatus,
			LastTestedAtUtc, LastEventAtUtc, LastSearchWatermarkAtUtc,
			LastSuccessAtUtc, CreatedAtUtc, UpdatedAtUtc, RetiredAtUtc
		) SELECT
			Id, Name, Host, Port, Channel, Timezone, Enabled, ArchiveEnabled,
			ArchiveStream, Model, Firmware, CapabilitiesJson, ConnectionStatus,
			LastTestedAtUtc, LastEventAtUtc, LastSearchWatermarkAtUtc,
			LastSuccessAtUtc, CreatedAtUtc, UpdatedAtUtc, RetiredAtUtc
		FROM Cameras`,
		`DROP TABLE Cameras`,
		`ALTER TABLE CamerasNext RENAME TO Cameras`,
		`CREATE UNIQUE INDEX IndexCamerasActiveName ON Cameras(Name) WHERE RetiredAtUtc IS NULL`,
		`CREATE UNIQUE INDEX IndexCamerasActiveHostChannel ON Cameras(Host, Channel) WHERE RetiredAtUtc IS NULL`,
		`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (2, CURRENT_TIMESTAMP)`,
	}
	for _, statement := range statements {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			transaction.Rollback()
			return fmt.Errorf("migrate active camera uniqueness: %w", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit camera uniqueness migration: %w", err)
	}
	if _, err := connection.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("enable foreign keys after migration: %w", err)
	}
	rows, err := connection.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check foreign keys after migration: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("camera uniqueness migration produced a foreign key violation")
	}
	return rows.Err()
}

func (store *Store) migrateArchiveSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS Recordings (
			Id TEXT PRIMARY KEY,
			CameraId TEXT NOT NULL REFERENCES Cameras(Id),
			NativeName TEXT NOT NULL,
			Stream TEXT NOT NULL CHECK (Stream IN ('Main', 'Sub')),
			StartAtUtc TEXT NOT NULL,
			EndAtUtc TEXT NOT NULL,
			SizeBytes INTEGER NOT NULL,
			State TEXT NOT NULL,
			RawMetadataJson TEXT NOT NULL,
			DiscoveredAtUtc TEXT NOT NULL,
			UpdatedAtUtc TEXT NOT NULL,
			LocalPath TEXT,
			Etag TEXT,
			Sha256 TEXT,
			CompletedAtUtc TEXT,
			DeletedAtUtc TEXT,
			UNIQUE(CameraId, NativeName)
		)`,
		`CREATE INDEX IF NOT EXISTS IndexRecordingsCameraStart ON Recordings(CameraId, StartAtUtc)`,
		`CREATE INDEX IF NOT EXISTS IndexRecordingsState ON Recordings(State, StartAtUtc)`,
		`CREATE TABLE IF NOT EXISTS RecordingTags (
			RecordingId TEXT NOT NULL REFERENCES Recordings(Id) ON DELETE CASCADE,
			EventType TEXT NOT NULL,
			Source TEXT NOT NULL,
			PRIMARY KEY(RecordingId, EventType, Source)
		)`,
		`CREATE TABLE IF NOT EXISTS Events (
			Id TEXT PRIMARY KEY,
			CameraId TEXT NOT NULL REFERENCES Cameras(Id),
			EventType TEXT NOT NULL,
			StartedAtUtc TEXT NOT NULL,
			EndedAtUtc TEXT,
			NativeRecordingName TEXT,
			RawPayloadJson TEXT NOT NULL,
			CreatedAtUtc TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS IndexEventsCameraStart ON Events(CameraId, StartedAtUtc)`,
		`CREATE TABLE IF NOT EXISTS IngestionRuns (
			Id TEXT PRIMARY KEY,
			CameraId TEXT NOT NULL REFERENCES Cameras(Id),
			Kind TEXT NOT NULL,
			WindowStartUtc TEXT NOT NULL,
			WindowEndUtc TEXT NOT NULL,
			Status TEXT NOT NULL,
			DiscoveredCount INTEGER NOT NULL,
			StartedAtUtc TEXT NOT NULL,
			CompletedAtUtc TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS IndexIngestionRunsCameraStarted ON IngestionRuns(CameraId, StartedAtUtc)`,
		`INSERT OR IGNORE INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (3, CURRENT_TIMESTAMP)`,
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate archive schema: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) migrateRecordingPolicies(ctx context.Context) error {
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SchemaMigrations WHERE Version = 4`).Scan(&applied); err != nil {
		return fmt.Errorf("check recording policy migration: %w", err)
	}
	if applied != 0 {
		return nil
	}
	statements := []string{
		`ALTER TABLE Cameras ADD COLUMN RecordingSource TEXT NOT NULL DEFAULT 'CameraStorage' CHECK (RecordingSource IN ('CameraStorage', 'Rtsp'))`,
		`ALTER TABLE Cameras ADD COLUMN RecordingMode TEXT NOT NULL DEFAULT 'Events' CHECK (RecordingMode IN ('Events', 'Continuous'))`,
		`ALTER TABLE Cameras ADD COLUMN HistoricalImport TEXT NOT NULL DEFAULT 'NewOnly' CHECK (HistoricalImport IN ('NewOnly', 'Last24Hours', 'Last48Hours', 'Last7Days', 'AllAvailable'))`,
		`ALTER TABLE Cameras ADD COLUMN PreEventSeconds INTEGER NOT NULL DEFAULT 10`,
		`ALTER TABLE Cameras ADD COLUMN PostEventSeconds INTEGER NOT NULL DEFAULT 15`,
		`ALTER TABLE Cameras ADD COLUMN ArchiveFromUtc TEXT`,
		`ALTER TABLE Recordings ADD COLUMN Source TEXT NOT NULL DEFAULT 'CameraStorage' CHECK (Source IN ('CameraStorage', 'Rtsp'))`,
		`ALTER TABLE Recordings ADD COLUMN Codec TEXT`,
		`UPDATE Cameras SET ArchiveFromUtc = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE ArchiveFromUtc IS NULL`,
		`UPDATE Recordings SET State = 'Discovered' WHERE State = 'Queued' AND LocalPath IS NULL`,
		`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (4, CURRENT_TIMESTAMP)`,
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate recording policies: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) migrateApplicationSettings(ctx context.Context) error {
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SchemaMigrations WHERE Version = 5`).Scan(&applied); err != nil {
		return fmt.Errorf("check application settings migration: %w", err)
	}
	if applied != 0 {
		return nil
	}
	statements := []string{
		`CREATE TABLE ApplicationSettings (
			Id INTEGER PRIMARY KEY CHECK (Id = 1),
			RecordingEnabled INTEGER NOT NULL CHECK (RecordingEnabled IN (0, 1)),
			UpdatedAtUtc TEXT NOT NULL
		)`,
		`INSERT INTO ApplicationSettings (Id, RecordingEnabled, UpdatedAtUtc)
		 VALUES (1, 1, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`,
		`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (5, CURRENT_TIMESTAMP)`,
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate application settings: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) migrateAutomaticCameraStorageImport(ctx context.Context) error {
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SchemaMigrations WHERE Version = 6`).Scan(&applied); err != nil {
		return fmt.Errorf("check automatic camera-storage import migration: %w", err)
	}
	if applied != 0 {
		return nil
	}
	statements := []string{
		`UPDATE Cameras SET HistoricalImport = 'AllAvailable', ArchiveFromUtc = NULL,
			LastSearchWatermarkAtUtc = NULL
			WHERE RecordingSource = 'CameraStorage' AND RetiredAtUtc IS NULL`,
		`UPDATE Recordings SET State = 'Queued', UpdatedAtUtc = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE Source = 'CameraStorage' AND State = 'Discovered' AND EXISTS (
				SELECT 1 FROM Cameras
				WHERE Cameras.Id = Recordings.CameraId AND Cameras.RetiredAtUtc IS NULL
				AND Cameras.ArchiveEnabled = 1 AND Cameras.RecordingSource = 'CameraStorage'
				AND (Cameras.RecordingMode = 'Continuous' OR EXISTS (
					SELECT 1 FROM RecordingTags
					JOIN CameraEventTypes ON CameraEventTypes.CameraId = Cameras.Id
						AND CameraEventTypes.EventType = RecordingTags.EventType
					WHERE RecordingTags.RecordingId = Recordings.Id AND CameraEventTypes.Enabled = 1
				))
			)`,
		`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (6, CURRENT_TIMESTAMP)`,
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate automatic camera-storage import: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) migrateStorageSettings(ctx context.Context) error {
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SchemaMigrations WHERE Version = 7`).Scan(&applied); err != nil {
		return fmt.Errorf("check storage settings migration: %w", err)
	}
	if applied != 0 {
		return nil
	}
	statements := []string{
		`ALTER TABLE ApplicationSettings ADD COLUMN RecordingFolder TEXT NOT NULL DEFAULT '.'`,
		`ALTER TABLE ApplicationSettings ADD COLUMN ArchiveLayout TEXT NOT NULL DEFAULT 'CameraDate'
			CHECK (ArchiveLayout IN ('CameraDate', 'DateCamera', 'Camera', 'Date'))`,
		`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (7, CURRENT_TIMESTAMP)`,
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate storage settings: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) GetRecordingControl(ctx context.Context) (domain.RecordingControl, error) {
	var enabled int
	var updated string
	if err := store.db.QueryRowContext(ctx, `SELECT RecordingEnabled, UpdatedAtUtc FROM ApplicationSettings WHERE Id = 1`).Scan(&enabled, &updated); err != nil {
		return domain.RecordingControl{}, fmt.Errorf("get recording control: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return domain.RecordingControl{}, fmt.Errorf("parse recording control update time: %w", err)
	}
	return domain.RecordingControl{Enabled: enabled == 1, UpdatedAtUtc: updatedAt}, nil
}

func (store *Store) SetRecordingControl(ctx context.Context, enabled bool, updatedAt time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE ApplicationSettings SET RecordingEnabled = ?, UpdatedAtUtc = ? WHERE Id = 1`, enabled, formatTime(&updatedAt)); err != nil {
			return err
		}
		if !enabled {
			_, err := tx.ExecContext(ctx, `UPDATE RecoveryPeriods SET EndedAtUtc = ? WHERE EndedAtUtc IS NULL`, formatTime(&updatedAt))
			return err
		}
		return nil
	})
}

func (store *Store) GetApplicationSettings(ctx context.Context) (domain.ApplicationSettings, error) {
	var settings domain.ApplicationSettings
	var updated string
	if err := store.db.QueryRowContext(ctx, `SELECT RecordingFolder, ArchiveLayout, UpdatedAtUtc FROM ApplicationSettings WHERE Id = 1`).Scan(
		&settings.RecordingFolder, &settings.ArchiveLayout, &updated); err != nil {
		return domain.ApplicationSettings{}, fmt.Errorf("get application settings: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return domain.ApplicationSettings{}, fmt.Errorf("parse application settings update time: %w", err)
	}
	settings.UpdatedAtUtc = updatedAt
	return settings, nil
}

func (store *Store) SetApplicationSettings(ctx context.Context, settings domain.ApplicationSettings) error {
	_, err := store.db.ExecContext(ctx, `UPDATE ApplicationSettings SET RecordingFolder = ?, ArchiveLayout = ?, UpdatedAtUtc = ? WHERE Id = 1`,
		settings.RecordingFolder, settings.ArchiveLayout, formatTime(&settings.UpdatedAtUtc))
	if err != nil {
		return fmt.Errorf("set application settings: %w", err)
	}
	return nil
}

func (store *Store) CreateCamera(ctx context.Context, camera domain.Camera, credential domain.EncryptedCredential, supported []domain.EventType) error {
	applyCameraPolicyDefaults(&camera)
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		capabilities, err := json.Marshal(camera.Capabilities)
		if err != nil {
			return fmt.Errorf("marshal capabilities: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO Cameras (
			Id, Name, Host, Port, Channel, Timezone, Enabled, ArchiveEnabled,
			ArchiveStream, RecordingSource, RecordingMode, HistoricalImport,
			PreEventSeconds, PostEventSeconds, ArchiveFromUtc, Model, Firmware,
			CapabilitiesJson, ConnectionStatus, LastTestedAtUtc, CreatedAtUtc, UpdatedAtUtc, RecoveryEnabled
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			camera.ID, camera.Name, camera.Host, camera.Port, camera.Channel,
			camera.Timezone, camera.Enabled, camera.ArchiveEnabled, camera.ArchiveStream,
			camera.RecordingSource, camera.RecordingMode, camera.HistoricalImport,
			camera.PreEventSeconds, camera.PostEventSeconds, formatTime(camera.ArchiveFromUtc),
			camera.Model, camera.Firmware, string(capabilities), camera.ConnectionStatus,
			formatTime(camera.LastTestedAtUtc), formatTime(&camera.CreatedAtUtc), formatTime(&camera.UpdatedAtUtc), camera.RecoveryEnabled)
		if err != nil {
			return fmt.Errorf("insert camera: %w", err)
		}
		if err := upsertCredential(ctx, tx, camera.ID, credential, camera.UpdatedAtUtc); err != nil {
			return err
		}
		return replaceEventTypes(ctx, tx, camera.ID, camera.EventTypes, supported)
	})
}

func (store *Store) UpdateCamera(ctx context.Context, camera domain.Camera, credential *domain.EncryptedCredential, supported []domain.EventType) error {
	applyCameraPolicyDefaults(&camera)
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		var previousTimezone string
		var previousSource domain.RecordingSource
		var previousMode domain.RecordingMode
		var previousHistoricalImport domain.HistoricalImport
		var previousStream domain.ArchiveStream
		if err := tx.QueryRowContext(ctx, `SELECT Timezone, RecordingSource, RecordingMode, HistoricalImport, ArchiveStream
			FROM Cameras WHERE Id = ? AND RetiredAtUtc IS NULL`, camera.ID).Scan(
			&previousTimezone, &previousSource, &previousMode, &previousHistoricalImport, &previousStream,
		); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("read camera policy: %w", err)
		}
		eventTypesChanged, err := enabledEventTypesChanged(ctx, tx, camera.ID, camera.EventTypes)
		if err != nil {
			return err
		}
		resetSearch := camera.RecordingSource == domain.RecordingSourceCameraStorage &&
			(previousSource != camera.RecordingSource || previousMode != camera.RecordingMode ||
				previousHistoricalImport != camera.HistoricalImport || previousStream != camera.ArchiveStream || eventTypesChanged)
		capabilities, err := json.Marshal(camera.Capabilities)
		if err != nil {
			return fmt.Errorf("marshal capabilities: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE Cameras SET
			Name = ?, Host = ?, Port = ?, Channel = ?, Timezone = ?, Enabled = ?,
			ArchiveEnabled = ?, ArchiveStream = ?, RecordingSource = ?, RecordingMode = ?,
			HistoricalImport = ?, PreEventSeconds = ?, PostEventSeconds = ?, ArchiveFromUtc = ?, Model = ?, Firmware = ?,
			CapabilitiesJson = ?, ConnectionStatus = ?, LastTestedAtUtc = ?, UpdatedAtUtc = ?,
			LastSearchWatermarkAtUtc = CASE WHEN ? THEN NULL ELSE LastSearchWatermarkAtUtc END, RecoveryEnabled = ?
			WHERE Id = ? AND RetiredAtUtc IS NULL`,
			camera.Name, camera.Host, camera.Port, camera.Channel, camera.Timezone,
			camera.Enabled, camera.ArchiveEnabled, camera.ArchiveStream,
			camera.RecordingSource, camera.RecordingMode, camera.HistoricalImport,
			camera.PreEventSeconds, camera.PostEventSeconds, formatTime(camera.ArchiveFromUtc),
			camera.Model, camera.Firmware, string(capabilities), camera.ConnectionStatus,
			formatTime(camera.LastTestedAtUtc), formatTime(&camera.UpdatedAtUtc), resetSearch, camera.RecoveryEnabled, camera.ID)
		if err != nil {
			return fmt.Errorf("update camera: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read updated camera count: %w", err)
		}
		if rows == 0 {
			return ErrNotFound
		}
		if resetSearch {
			if _, err := tx.ExecContext(ctx, `DELETE FROM IngestionRuns WHERE CameraId = ? AND Kind = 'Backfill'`, camera.ID); err != nil {
				return fmt.Errorf("reset camera-storage backfill cursor: %w", err)
			}
		}
		if credential != nil {
			if err := upsertCredential(ctx, tx, camera.ID, *credential, camera.UpdatedAtUtc); err != nil {
				return err
			}
		}
		if err := reinterpretDiscoveredRecordingTimes(ctx, tx, camera.ID, camera.Timezone, camera.UpdatedAtUtc, camera.Timezone != previousTimezone); err != nil {
			return err
		}
		if err := replaceEventTypes(ctx, tx, camera.ID, camera.EventTypes, supported); err != nil {
			return err
		}
		if camera.RecordingSource == domain.RecordingSourceCameraStorage && camera.ArchiveEnabled {
			return queueEligibleDiscovered(ctx, tx, camera.ID, camera.RecordingMode, camera.UpdatedAtUtc)
		}
		return nil
	})
}

func queueEligibleDiscovered(ctx context.Context, tx *sql.Tx, cameraID string, mode domain.RecordingMode, updatedAt time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE Recordings SET State = 'Queued', UpdatedAtUtc = ?
		WHERE CameraId = ? AND Source = 'CameraStorage' AND State = 'Discovered'
		AND (? = 'Continuous' OR EXISTS (
			SELECT 1 FROM RecordingTags
			JOIN CameraEventTypes ON CameraEventTypes.CameraId = ?
				AND CameraEventTypes.EventType = RecordingTags.EventType
			WHERE RecordingTags.RecordingId = Recordings.Id AND CameraEventTypes.Enabled = 1
		))`, formatTime(&updatedAt), cameraID, mode, cameraID)
	if err != nil {
		return fmt.Errorf("queue discovered camera-storage recordings: %w", err)
	}
	return nil
}

func reinterpretDiscoveredRecordingTimes(ctx context.Context, tx *sql.Tx, cameraID, timezone string, updatedAt time.Time, includeDiscovered bool) error {
	if timezone == "Camera/Unverified" {
		return nil
	}
	_, err := reolink.CameraLocation(timezone)
	if err != nil {
		return errors.New("camera timezone is invalid")
	}
	estimatedClock := reolink.CameraClockEstimated(timezone)
	rows, err := tx.QueryContext(ctx, `SELECT Id, RawMetadataJson, StartAtUtc, State, COALESCE(LocalPath, '') FROM Recordings
		WHERE CameraId = ? AND Source = 'CameraStorage'
		AND ((State = 'Discovered' AND ?) OR (COALESCE(json_extract(CASE WHEN json_valid(RawMetadataJson) THEN RawMetadataJson ELSE '{}' END,
		'$._reonvrEstimatedClock'), 0) = 1
		AND State NOT IN ('Downloading', 'Verifying')))`, cameraID, includeDiscovered)
	if err != nil {
		return fmt.Errorf("list discovered recording timestamps: %w", err)
	}
	type recordingTime struct {
		id          string
		start       time.Time
		end         time.Time
		previous    time.Time
		metadata    string
		revalidated bool
	}
	updates := make([]recordingTime, 0)
	for rows.Next() {
		var id, rawMetadata, previousStart, state, localPath string
		if err := rows.Scan(&id, &rawMetadata, &previousStart, &state, &localPath); err != nil {
			rows.Close()
			return fmt.Errorf("scan discovered recording timestamp: %w", err)
		}
		var metadata struct {
			StartTime string `json:"startTime"`
			EndTime   string `json:"endTime"`
			Estimated bool   `json:"_reonvrEstimatedClock"`
		}
		if err := json.Unmarshal([]byte(rawMetadata), &metadata); err != nil {
			continue
		}
		if metadata.Estimated && estimatedClock && state != string(domain.RecordingDiscovered) {
			continue
		}
		start, end, err := reolink.CameraRecordingTimes(timezone, []byte(rawMetadata))
		if err != nil {
			continue
		}
		previous, err := time.Parse(time.RFC3339Nano, previousStart)
		if err != nil {
			rows.Close()
			return err
		}
		if metadata.Estimated || estimatedClock {
			var values map[string]json.RawMessage
			if err := json.Unmarshal([]byte(rawMetadata), &values); err != nil {
				rows.Close()
				return err
			}
			if estimatedClock {
				values["_reonvrEstimatedClock"] = json.RawMessage("true")
			} else {
				delete(values, "_reonvrEstimatedClock")
				if state == string(domain.RecordingComplete) && localPath != "" {
					values["_reonvrSidecarPending"] = json.RawMessage("true")
				}
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				return err
			}
			rawMetadata = string(encoded)
		}
		updates = append(updates, recordingTime{id: id, start: start, end: end, previous: previous, metadata: rawMetadata,
			revalidated: metadata.Estimated && !estimatedClock})
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close discovered recording timestamps: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list discovered recording timestamps: %w", err)
	}
	for _, update := range updates {
		if update.revalidated {
			if err := reinterpretEstimatedCoverage(ctx, tx, update.id, update.previous, update.start, update.end); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE Recordings SET StartAtUtc = ?, EndAtUtc = ?, RawMetadataJson = ?, UpdatedAtUtc = ? WHERE Id = ?`,
			formatTime(&update.start), formatTime(&update.end), update.metadata, formatTime(&updatedAt), update.id); err != nil {
			return fmt.Errorf("update discovered recording timestamp: %w", err)
		}
	}
	return nil
}

func reinterpretEstimatedCoverage(ctx context.Context, tx *sql.Tx, recordingID string, previous, start, end time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT StartAtUtc, EndAtUtc FROM RecordingCoverage WHERE RecordingId = ?`, recordingID)
	if err != nil {
		return err
	}
	var corrected []domain.TimeRange
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			rows.Close()
			return err
		}
		coverageStart, err := time.Parse(time.RFC3339Nano, from)
		if err != nil {
			rows.Close()
			return err
		}
		coverageEnd, err := time.Parse(time.RFC3339Nano, to)
		if err != nil {
			rows.Close()
			return err
		}
		coverageStart = coverageStart.Add(start.Sub(previous))
		coverageEnd = coverageEnd.Add(start.Sub(previous))
		if coverageStart.Before(start) {
			coverageStart = start
		}
		if coverageEnd.After(end) {
			coverageEnd = end
		}
		if coverageStart.Before(coverageEnd) {
			corrected = append(corrected, domain.TimeRange{StartAtUtc: coverageStart, EndAtUtc: coverageEnd})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return saveRecordingCoverageTx(ctx, tx, recordingID, corrected)
}

func (store *Store) ListCameras(ctx context.Context) ([]domain.Camera, error) {
	rows, err := store.db.QueryContext(ctx, cameraSelect+` ORDER BY Cameras.Name`)
	if err != nil {
		return nil, fmt.Errorf("list cameras: %w", err)
	}

	cameras := make([]domain.Camera, 0)
	for rows.Next() {
		camera, err := scanCamera(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		cameras = append(cameras, camera)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close camera rows: %w", err)
	}
	for index := range cameras {
		if err := store.loadEventTypes(ctx, &cameras[index]); err != nil {
			return nil, err
		}
		if err := store.loadArchiveStats(ctx, &cameras[index]); err != nil {
			return nil, err
		}
	}
	return cameras, nil
}

func (store *Store) GetCamera(ctx context.Context, id string) (domain.Camera, error) {
	camera, err := scanCamera(store.db.QueryRowContext(ctx, cameraSelect+` AND Cameras.Id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Camera{}, ErrNotFound
	}
	if err != nil {
		return domain.Camera{}, err
	}
	if err := store.loadEventTypes(ctx, &camera); err != nil {
		return domain.Camera{}, err
	}
	if err := store.loadArchiveStats(ctx, &camera); err != nil {
		return domain.Camera{}, err
	}
	return camera, nil
}

func (store *Store) GetCredential(ctx context.Context, cameraID string) (domain.EncryptedCredential, error) {
	var credential domain.EncryptedCredential
	err := store.db.QueryRowContext(ctx, `SELECT Username, PasswordCiphertext, PasswordNonce, KeyVersion FROM CameraCredentials WHERE CameraId = ?`, cameraID).Scan(
		&credential.Username, &credential.Ciphertext, &credential.Nonce, &credential.KeyVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.EncryptedCredential{}, ErrNotFound
	}
	if err != nil {
		return domain.EncryptedCredential{}, fmt.Errorf("get camera credential: %w", err)
	}
	return credential, nil
}

func (store *Store) RetireCamera(ctx context.Context, id string, now time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE Cameras SET Enabled = 0, ConnectionStatus = 'Offline', RetiredAtUtc = ?, UpdatedAtUtc = ? WHERE Id = ? AND RetiredAtUtc IS NULL`, formatTime(&now), formatTime(&now), id)
		if err != nil {
			return fmt.Errorf("retire camera: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read retired camera count: %w", err)
		}
		if rows == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM CameraCredentials WHERE CameraId = ?`, id); err != nil {
			return fmt.Errorf("delete camera credential: %w", err)
		}
		return nil
	})
}

const cameraSelect = `SELECT
	Cameras.Id, Cameras.Name, Cameras.Host, Cameras.Port, Cameras.Channel,
	Cameras.Timezone, CameraCredentials.Username, Cameras.Enabled,
	Cameras.ArchiveEnabled, Cameras.ArchiveStream, Cameras.RecordingSource,
	Cameras.RecordingMode, Cameras.HistoricalImport, Cameras.PreEventSeconds,
	Cameras.PostEventSeconds, Cameras.ArchiveFromUtc, Cameras.Model,
	Cameras.Firmware, Cameras.CapabilitiesJson, Cameras.ConnectionStatus,
	Cameras.LastTestedAtUtc, Cameras.LastEventAtUtc, Cameras.CreatedAtUtc, Cameras.UpdatedAtUtc, Cameras.RecoveryEnabled
	FROM Cameras JOIN CameraCredentials ON CameraCredentials.CameraId = Cameras.Id
	WHERE Cameras.RetiredAtUtc IS NULL`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCamera(row rowScanner) (domain.Camera, error) {
	var camera domain.Camera
	var enabled, archiveEnabled int
	var model, firmware, capabilitiesJSON, lastTested, lastEvent, archiveFrom sql.NullString
	var created, updated string
	if err := row.Scan(&camera.ID, &camera.Name, &camera.Host, &camera.Port, &camera.Channel,
		&camera.Timezone, &camera.Username, &enabled, &archiveEnabled, &camera.ArchiveStream,
		&camera.RecordingSource, &camera.RecordingMode, &camera.HistoricalImport,
		&camera.PreEventSeconds, &camera.PostEventSeconds, &archiveFrom,
		&model, &firmware, &capabilitiesJSON, &camera.ConnectionStatus, &lastTested,
		&lastEvent, &created, &updated, &camera.RecoveryEnabled); err != nil {
		return domain.Camera{}, err
	}
	camera.Enabled = enabled == 1
	camera.ArchiveEnabled = archiveEnabled == 1
	camera.Model = model.String
	camera.Firmware = firmware.String
	camera.CredentialConfigured = true
	if capabilitiesJSON.Valid && capabilitiesJSON.String != "null" {
		if err := json.Unmarshal([]byte(capabilitiesJSON.String), &camera.Capabilities); err != nil {
			return domain.Camera{}, fmt.Errorf("decode camera capabilities: %w", err)
		}
	}
	if lastTested.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, lastTested.String)
		if err != nil {
			return domain.Camera{}, fmt.Errorf("parse last tested time: %w", err)
		}
		camera.LastTestedAtUtc = &parsed
	}
	if lastEvent.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, lastEvent.String)
		if err != nil {
			return domain.Camera{}, fmt.Errorf("parse last event time: %w", err)
		}
		camera.LastEventAtUtc = &parsed
	}
	if archiveFrom.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, archiveFrom.String)
		if err != nil {
			return domain.Camera{}, fmt.Errorf("parse archive boundary: %w", err)
		}
		camera.ArchiveFromUtc = &parsed
	}
	var err error
	camera.CreatedAtUtc, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return domain.Camera{}, fmt.Errorf("parse created time: %w", err)
	}
	camera.UpdatedAtUtc, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return domain.Camera{}, fmt.Errorf("parse updated time: %w", err)
	}
	return camera, nil
}

func (store *Store) loadEventTypes(ctx context.Context, camera *domain.Camera) error {
	rows, err := store.db.QueryContext(ctx, `SELECT EventType FROM CameraEventTypes WHERE CameraId = ? AND Enabled = 1 ORDER BY EventType`, camera.ID)
	if err != nil {
		return fmt.Errorf("list camera event types: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventType domain.EventType
		if err := rows.Scan(&eventType); err != nil {
			return fmt.Errorf("scan camera event type: %w", err)
		}
		camera.EventTypes = append(camera.EventTypes, eventType)
	}
	return rows.Err()
}

func (store *Store) loadArchiveStats(ctx context.Context, camera *domain.Camera) error {
	err := store.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN State = 'Complete' AND LocalPath IS NOT NULL AND LocalPath != '' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN State IN ('Queued', 'Downloading', 'Verifying', 'RetryWait') THEN 1 ELSE 0 END), 0)
		FROM Recordings WHERE CameraId = ? AND State != 'Deleted'`, camera.ID).Scan(
		&camera.RecordingCount, &camera.PendingDownloadCount)
	if err != nil {
		return fmt.Errorf("load camera archive statistics: %w", err)
	}
	return store.loadRecoveryStats(ctx, camera)
}

func (store *Store) withTransaction(ctx context.Context, action func(*sql.Tx) error) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := action(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func upsertCredential(ctx context.Context, tx *sql.Tx, cameraID string, credential domain.EncryptedCredential, updated time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO CameraCredentials (CameraId, Username, PasswordCiphertext, PasswordNonce, KeyVersion, UpdatedAtUtc)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(CameraId) DO UPDATE SET Username = excluded.Username, PasswordCiphertext = excluded.PasswordCiphertext,
		PasswordNonce = excluded.PasswordNonce, KeyVersion = excluded.KeyVersion, UpdatedAtUtc = excluded.UpdatedAtUtc`,
		cameraID, credential.Username, credential.Ciphertext, credential.Nonce, credential.KeyVersion, formatTime(&updated))
	if err != nil {
		return fmt.Errorf("upsert camera credential: %w", err)
	}
	return nil
}

func replaceEventTypes(ctx context.Context, tx *sql.Tx, cameraID string, enabled, supported []domain.EventType) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM CameraEventTypes WHERE CameraId = ?`, cameraID); err != nil {
		return fmt.Errorf("clear camera event types: %w", err)
	}
	enabledSet := make(map[domain.EventType]bool, len(enabled))
	for _, eventType := range enabled {
		enabledSet[eventType] = true
	}
	for _, eventType := range supported {
		if _, err := tx.ExecContext(ctx, `INSERT INTO CameraEventTypes (CameraId, EventType, Enabled, Supported) VALUES (?, ?, ?, 1)`, cameraID, eventType, enabledSet[eventType]); err != nil {
			return fmt.Errorf("insert camera event type: %w", err)
		}
	}
	return nil
}

func enabledEventTypesChanged(ctx context.Context, tx *sql.Tx, cameraID string, enabled []domain.EventType) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT EventType FROM CameraEventTypes WHERE CameraId = ? AND Enabled = 1`, cameraID)
	if err != nil {
		return false, fmt.Errorf("list enabled camera event types: %w", err)
	}
	defer rows.Close()
	current := make(map[domain.EventType]bool)
	for rows.Next() {
		var eventType domain.EventType
		if err := rows.Scan(&eventType); err != nil {
			return false, fmt.Errorf("scan enabled camera event type: %w", err)
		}
		current[eventType] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	desired := make(map[domain.EventType]bool, len(enabled))
	for _, eventType := range enabled {
		desired[eventType] = true
	}
	if len(current) != len(desired) {
		return true, nil
	}
	for eventType := range current {
		if !desired[eventType] {
			return true, nil
		}
	}
	return false, nil
}

func formatTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func (store *Store) UserSchemaIdentifiers(ctx context.Context) ([]string, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT Name FROM sqlite_schema WHERE Name NOT LIKE 'sqlite%' AND Type IN ('table', 'index')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var identifiers []string
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		identifiers = append(identifiers, name)
		if !strings.HasPrefix(name, "Index") {
			tables = append(tables, name)
		}
	}
	for _, table := range tables {
		columnRows, err := store.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
		if err != nil {
			return nil, err
		}
		for columnRows.Next() {
			var sequence, columnType, notNull, primaryKey int
			var name, declaredType string
			var defaultValue any
			if err := columnRows.Scan(&sequence, &name, &declaredType, &notNull, &defaultValue, &primaryKey); err != nil {
				columnRows.Close()
				return nil, err
			}
			identifiers = append(identifiers, name)
			_ = columnType
		}
		columnRows.Close()
	}
	return identifiers, nil
}

func (store *Store) UpdateCameraProbe(ctx context.Context, id, model, firmware, timezone string, capabilities domain.CameraCapabilities, status string, now time.Time) error {
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		return fmt.Errorf("marshal camera capabilities: %w", err)
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		var previousTimezone string
		if err := tx.QueryRowContext(ctx, `SELECT Timezone FROM Cameras WHERE Id = ? AND RetiredAtUtc IS NULL`, id).Scan(&previousTimezone); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("read camera timezone: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE Cameras SET Model = ?, Firmware = ?, Timezone = ?, CapabilitiesJson = ?, ConnectionStatus = ?, LastTestedAtUtc = ?, LastSuccessAtUtc = ?, UpdatedAtUtc = ? WHERE Id = ? AND RetiredAtUtc IS NULL`,
			model, firmware, timezone, string(encoded), status, formatTime(&now), formatTime(&now), formatTime(&now), id)
		if err != nil {
			return fmt.Errorf("update camera probe: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrNotFound
		}
		return reinterpretDiscoveredRecordingTimes(ctx, tx, id, timezone, now, timezone != previousTimezone)
	})
}

func (store *Store) UpdateCameraStatus(ctx context.Context, id, status string, now time.Time) error {
	_, err := store.db.ExecContext(ctx, `UPDATE Cameras SET ConnectionStatus = ?, UpdatedAtUtc = ? WHERE Id = ? AND RetiredAtUtc IS NULL`, status, formatTime(&now), id)
	if err != nil {
		return fmt.Errorf("update camera status: %w", err)
	}
	return nil
}

func (store *Store) GetSearchWatermark(ctx context.Context, cameraID string) (*time.Time, error) {
	var raw sql.NullString
	if err := store.db.QueryRowContext(ctx, `SELECT LastSearchWatermarkAtUtc FROM Cameras WHERE Id = ? AND RetiredAtUtc IS NULL`, cameraID).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("get search watermark: %w", err)
	}
	if !raw.Valid {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return nil, fmt.Errorf("parse search watermark: %w", err)
	}
	return &value, nil
}

func (store *Store) GetBackfillCursor(ctx context.Context, cameraID string) (*time.Time, int, error) {
	var raw string
	var discoveredCount int
	err := store.db.QueryRowContext(ctx, `SELECT WindowStartUtc, DiscoveredCount FROM IngestionRuns
		WHERE CameraId = ? AND Kind = 'Backfill' AND Status = 'Complete'
		ORDER BY julianday(WindowStartUtc) ASC LIMIT 1`, cameraID).Scan(&raw, &discoveredCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get camera-storage backfill cursor: %w", err)
	}
	cursor, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, 0, fmt.Errorf("parse camera-storage backfill cursor: %w", err)
	}
	return &cursor, discoveredCount, nil
}

func (store *Store) SaveReconciliation(ctx context.Context, run domain.IngestionRun, recordings []domain.Recording, watermark time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		return saveReconciliationTx(ctx, tx, run, recordings, watermark)
	})
}

func saveReconciliationTx(ctx context.Context, tx *sql.Tx, run domain.IngestionRun, recordings []domain.Recording, watermark time.Time) error {
	for _, recording := range recordings {
		if recording.Source == "" {
			recording.Source = domain.RecordingSourceCameraStorage
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO Recordings (
				Id, CameraId, NativeName, Source, Stream, Codec, StartAtUtc, EndAtUtc, SizeBytes,
				State, RawMetadataJson, DiscoveredAtUtc, UpdatedAtUtc
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(CameraId, NativeName) DO UPDATE SET
				Source = excluded.Source, Stream = excluded.Stream, Codec = excluded.Codec, StartAtUtc = excluded.StartAtUtc,
				EndAtUtc = excluded.EndAtUtc, SizeBytes = excluded.SizeBytes,
				State = CASE
					WHEN Recordings.State = 'Discovered' AND excluded.State = 'Queued' THEN 'Queued'
					ELSE Recordings.State END,
				RawMetadataJson = excluded.RawMetadataJson, UpdatedAtUtc = excluded.UpdatedAtUtc
				WHERE Recordings.State NOT IN ('Downloading', 'Verifying')
				AND COALESCE(json_extract(CASE WHEN json_valid(Recordings.RawMetadataJson) THEN Recordings.RawMetadataJson ELSE '{}' END,
				'$._reonvrSidecarPending'), 0) != 1`,
			recording.ID, recording.CameraID, recording.NativeName, recording.Source, recording.Stream, nullableString(recording.Codec),
			formatTime(&recording.StartAtUtc), formatTime(&recording.EndAtUtc), recording.SizeBytes,
			recording.State, string(recording.RawMetadata), formatTime(&recording.DiscoveredAtUtc),
			formatTime(&recording.UpdatedAtUtc))
		if err != nil {
			return fmt.Errorf("upsert recording: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM RecordingTags WHERE RecordingId = ? AND Source = 'Native'`, recording.ID); err != nil {
			return fmt.Errorf("clear recording tags: %w", err)
		}
		for _, eventType := range recording.EventTypes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO RecordingTags (RecordingId, EventType, Source) VALUES (?, ?, 'Native')`, recording.ID, eventType); err != nil {
				return fmt.Errorf("insert recording tag: %w", err)
			}
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO IngestionRuns (
			Id, CameraId, Kind, WindowStartUtc, WindowEndUtc, Status,
			DiscoveredCount, StartedAtUtc, CompletedAtUtc
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, run.CameraID, run.Kind,
		formatTime(&run.WindowStartUtc), formatTime(&run.WindowEndUtc), run.Status,
		run.DiscoveredCount, formatTime(&run.StartedAtUtc), formatTime(run.CompletedAtUtc))
	if err != nil {
		return fmt.Errorf("insert ingestion run: %w", err)
	}
	formattedWatermark := formatTime(&watermark)
	result, err := tx.ExecContext(ctx, `UPDATE Cameras SET
		LastSearchWatermarkAtUtc = CASE
			WHEN LastSearchWatermarkAtUtc IS NULL OR julianday(LastSearchWatermarkAtUtc) < julianday(?) THEN ?
			ELSE LastSearchWatermarkAtUtc END,
		UpdatedAtUtc = CASE WHEN julianday(UpdatedAtUtc) < julianday(?) THEN ? ELSE UpdatedAtUtc END
		WHERE Id = ? AND RetiredAtUtc IS NULL`,
		formattedWatermark, formattedWatermark, formattedWatermark, formattedWatermark, run.CameraID)
	if err != nil {
		return fmt.Errorf("update search watermark: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read watermark update count: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) ListEventsReadyForFinalization(ctx context.Context, now time.Time, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := store.db.QueryContext(ctx, `SELECT Events.Id, Events.CameraId, Events.EventType,
		Events.StartedAtUtc, Events.EndedAtUtc, Events.NativeRecordingName,
		Events.RawPayloadJson, Events.CreatedAtUtc
		FROM Events JOIN Cameras ON Cameras.Id = Events.CameraId
		WHERE Events.EndedAtUtc IS NOT NULL AND Events.NativeRecordingName IS NULL
		AND Cameras.RetiredAtUtc IS NULL AND Cameras.Enabled = 1 AND Cameras.ArchiveEnabled = 1
		AND Cameras.RecordingSource = 'CameraStorage' AND Cameras.RecordingMode = 'Events'
		AND julianday(Events.EndedAtUtc) + (15.0 / 86400.0) <= julianday(?)
		ORDER BY Events.EndedAtUtc ASC LIMIT ?`, formatTime(&now), limit)
	if err != nil {
		return nil, fmt.Errorf("list events ready for finalization: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (store *Store) SaveEventFinalization(ctx context.Context, eventID string, run domain.IngestionRun, recordings []domain.Recording, nativeName string, watermark time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		if err := saveReconciliationTx(ctx, tx, run, recordings, watermark); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE Events SET NativeRecordingName = ?
			WHERE Id = ? AND NativeRecordingName IS NULL`, nativeName, eventID)
		if err != nil {
			return fmt.Errorf("complete event finalization: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read finalized event count: %w", err)
		}
		if count == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (store *Store) MarkEventFinalizationMiss(ctx context.Context, eventID string) error {
	result, err := store.db.ExecContext(ctx, `UPDATE Events SET NativeRecordingName = ''
		WHERE Id = ? AND NativeRecordingName IS NULL`, eventID)
	if err != nil {
		return fmt.Errorf("complete event finalization without media: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read event finalization count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) ListRecordings(ctx context.Context, cameraID string, limit int) ([]domain.Recording, error) {
	return store.listRecordings(ctx, cameraID, limit, false)
}

func (store *Store) ListArchivedRecordings(ctx context.Context, cameraID string, limit int) ([]domain.Recording, error) {
	return store.listRecordings(ctx, cameraID, limit, true)
}

func (store *Store) listRecordings(ctx context.Context, cameraID string, limit int, archivedOnly bool) ([]domain.Recording, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT Id, CameraId, NativeName, Source, Stream, IFNULL(Codec, ''),
		StartAtUtc, EndAtUtc, SizeBytes, State, RawMetadataJson, DiscoveredAtUtc, UpdatedAtUtc,
		IFNULL(LocalPath, ''), IFNULL(Sha256, '')
		FROM Recordings WHERE CameraId = ? ORDER BY StartAtUtc DESC LIMIT ?`
	if archivedOnly {
		query = `SELECT Id, CameraId, NativeName, Source, Stream, IFNULL(Codec, ''),
			StartAtUtc, EndAtUtc, SizeBytes, State, RawMetadataJson, DiscoveredAtUtc, UpdatedAtUtc,
			IFNULL(LocalPath, ''), IFNULL(Sha256, '')
			FROM Recordings WHERE CameraId = ? AND State = 'Complete'
			AND LocalPath IS NOT NULL AND LocalPath != '' ORDER BY StartAtUtc DESC LIMIT ?`
	}
	rows, err := store.db.QueryContext(ctx, query, cameraID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", err)
	}
	defer rows.Close()
	recordings := make([]domain.Recording, 0)
	for rows.Next() {
		var recording domain.Recording
		var start, end, rawMetadata, discovered, updated string
		if err := rows.Scan(&recording.ID, &recording.CameraID, &recording.NativeName,
			&recording.Source, &recording.Stream, &recording.Codec, &start, &end,
			&recording.SizeBytes, &recording.State, &rawMetadata, &discovered, &updated,
			&recording.LocalPath, &recording.Sha256); err != nil {
			return nil, fmt.Errorf("scan recording: %w", err)
		}
		recording.RawMetadata = json.RawMessage(rawMetadata)
		var parseErr error
		if recording.StartAtUtc, parseErr = time.Parse(time.RFC3339Nano, start); parseErr != nil {
			return nil, fmt.Errorf("parse recording start: %w", parseErr)
		}
		if recording.EndAtUtc, parseErr = time.Parse(time.RFC3339Nano, end); parseErr != nil {
			return nil, fmt.Errorf("parse recording end: %w", parseErr)
		}
		if recording.DiscoveredAtUtc, parseErr = time.Parse(time.RFC3339Nano, discovered); parseErr != nil {
			return nil, fmt.Errorf("parse recording discovery time: %w", parseErr)
		}
		if recording.UpdatedAtUtc, parseErr = time.Parse(time.RFC3339Nano, updated); parseErr != nil {
			return nil, fmt.Errorf("parse recording update time: %w", parseErr)
		}
		recordings = append(recordings, recording)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close recording rows: %w", err)
	}
	for index := range recordings {
		tags, err := store.listRecordingTags(ctx, recordings[index].ID)
		if err != nil {
			return nil, err
		}
		recordings[index].EventTypes = tags
	}
	return recordings, nil
}

func (store *Store) GetRecording(ctx context.Context, id string) (domain.Recording, error) {
	row := store.db.QueryRowContext(ctx, `SELECT Id, CameraId, NativeName, Source, Stream, IFNULL(Codec, ''),
		StartAtUtc, EndAtUtc, SizeBytes, State, RawMetadataJson, DiscoveredAtUtc, UpdatedAtUtc,
		IFNULL(LocalPath, ''), IFNULL(Sha256, '') FROM Recordings WHERE Id = ?`, id)
	recording, err := scanRecording(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Recording{}, ErrNotFound
	}
	if err != nil {
		return domain.Recording{}, fmt.Errorf("get recording: %w", err)
	}
	recording.EventTypes, err = store.listRecordingTags(ctx, recording.ID)
	if err != nil {
		return domain.Recording{}, err
	}
	return recording, nil
}

func (store *Store) ClaimNextDownload(ctx context.Context, cameraID string, now time.Time) (*domain.Recording, error) {
	row := store.db.QueryRowContext(ctx, `UPDATE Recordings SET State = 'Downloading', UpdatedAtUtc = ?
		WHERE Id = (
			SELECT Id FROM Recordings
			WHERE CameraId = ? AND Source = 'CameraStorage' AND State IN ('Queued', 'RetryWait')
			AND (EXISTS (SELECT 1 FROM Cameras WHERE Cameras.Id = Recordings.CameraId AND Cameras.RecordingSource = 'CameraStorage')
			OR EXISTS (SELECT 1 FROM RecoveryGapRecordings Links JOIN RecoveryGaps Gaps ON Gaps.Id = Links.GapId
				WHERE Links.RecordingId = Recordings.Id AND Gaps.Status NOT IN ('Recovered', 'RecoveredEvents', 'CheckedEvents')))
			ORDER BY StartAtUtc DESC LIMIT 1
		)
		RETURNING Id, CameraId, NativeName, Source, Stream, IFNULL(Codec, ''),
			StartAtUtc, EndAtUtc, SizeBytes, State, RawMetadataJson, DiscoveredAtUtc,
			UpdatedAtUtc, IFNULL(LocalPath, ''), IFNULL(Sha256, '')`, formatTime(&now), cameraID)
	recording, err := scanRecording(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim recording download: %w", err)
	}
	recording.EventTypes, err = store.listRecordingTags(ctx, recording.ID)
	if err != nil {
		return nil, err
	}
	return &recording, nil
}

func (store *Store) UpdateRecordingState(ctx context.Context, id string, state domain.RecordingState, now time.Time) error {
	result, err := store.db.ExecContext(ctx, `UPDATE Recordings SET State = ?, UpdatedAtUtc = ? WHERE Id = ?`, state, formatTime(&now), id)
	if err != nil {
		return fmt.Errorf("update recording state: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read recording state update count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) CompleteRecording(ctx context.Context, id, localPath, sha256 string, sizeBytes int64, now time.Time) error {
	result, err := store.db.ExecContext(ctx, `UPDATE Recordings SET State = 'Complete', LocalPath = ?, Sha256 = ?,
		SizeBytes = ?, CompletedAtUtc = ?, UpdatedAtUtc = ? WHERE Id = ?`,
		localPath, sha256, sizeBytes, formatTime(&now), formatTime(&now), id)
	if err != nil {
		return fmt.Errorf("complete recording: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read completed recording count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) SaveRtspRecording(ctx context.Context, recording domain.Recording) error {
	if recording.Source != domain.RecordingSourceRtsp || recording.State != domain.RecordingComplete {
		return errors.New("RTSP recording must be complete before it is indexed")
	}
	if len(recording.RawMetadata) == 0 {
		recording.RawMetadata = json.RawMessage(`{}`)
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO Recordings (
			Id, CameraId, NativeName, Source, Stream, Codec, StartAtUtc, EndAtUtc,
			SizeBytes, State, RawMetadataJson, DiscoveredAtUtc, UpdatedAtUtc,
			LocalPath, Sha256, CompletedAtUtc
		) VALUES (?, ?, ?, 'Rtsp', ?, ?, ?, ?, ?, 'Complete', ?, ?, ?, ?, ?, ?)`,
			recording.ID, recording.CameraID, recording.NativeName, recording.Stream,
			nullableString(recording.Codec), formatTime(&recording.StartAtUtc), formatTime(&recording.EndAtUtc),
			recording.SizeBytes, string(recording.RawMetadata), formatTime(&recording.DiscoveredAtUtc),
			formatTime(&recording.UpdatedAtUtc), recording.LocalPath, recording.Sha256,
			formatTime(&recording.UpdatedAtUtc))
		if err != nil {
			return fmt.Errorf("insert RTSP recording: %w", err)
		}
		for _, eventType := range recording.EventTypes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO RecordingTags (RecordingId, EventType, Source) VALUES (?, ?, 'Rtsp')`, recording.ID, eventType); err != nil {
				return fmt.Errorf("insert RTSP recording tag: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) ListRetentionCandidates(ctx context.Context, limit int) ([]domain.Recording, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := store.db.QueryContext(ctx, `SELECT Id, CameraId, NativeName, Source, Stream, IFNULL(Codec, ''),
		StartAtUtc, EndAtUtc, SizeBytes, State, RawMetadataJson, DiscoveredAtUtc, UpdatedAtUtc,
		IFNULL(LocalPath, ''), IFNULL(Sha256, '')
		FROM Recordings WHERE State = 'Complete' AND LocalPath IS NOT NULL
		ORDER BY StartAtUtc ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list retention candidates: %w", err)
	}
	defer rows.Close()
	recordings := make([]domain.Recording, 0)
	for rows.Next() {
		recording, err := scanRecording(rows)
		if err != nil {
			return nil, fmt.Errorf("scan retention candidate: %w", err)
		}
		recordings = append(recordings, recording)
	}
	return recordings, rows.Err()
}

func (store *Store) MarkRecordingDeleted(ctx context.Context, id string, now time.Time) error {
	result, err := store.db.ExecContext(ctx, `UPDATE Recordings SET State = 'Deleted',
		RawMetadataJson = CASE WHEN json_valid(RawMetadataJson) THEN json_remove(RawMetadataJson, '$._reonvrSidecarPending') ELSE RawMetadataJson END,
		LocalPath = NULL, DeletedAtUtc = ?, UpdatedAtUtc = ? WHERE Id = ? AND State = 'Complete'`,
		formatTime(&now), formatTime(&now), id)
	if err != nil {
		return fmt.Errorf("tombstone recording: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read tombstoned recording count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) ApplyEventTransition(ctx context.Context, cameraID string, transition domain.EventTransition) error {
	if strings.TrimSpace(cameraID) == "" || transition.OccurredAtUtc.IsZero() {
		return errors.New("camera and event timestamp are required")
	}
	if transition.Active && !validEventType(transition.EventType) {
		return errors.New("active event type is invalid")
	}
	if len(transition.RawPayload) == 0 {
		transition.RawPayload = json.RawMessage(`{}`)
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		if transition.Active {
			digest := sha256.Sum256([]byte(cameraID + "\x00" + string(transition.EventType) + "\x00" + transition.OccurredAtUtc.Format(time.RFC3339Nano)))
			id := fmt.Sprintf("%x", digest[:16])
			_, err := tx.ExecContext(ctx, `INSERT INTO Events (
				Id, CameraId, EventType, StartedAtUtc, RawPayloadJson, CreatedAtUtc
			) SELECT ?, ?, ?, ?, ?, ? WHERE NOT EXISTS (
				SELECT 1 FROM Events WHERE CameraId = ? AND EventType = ? AND EndedAtUtc IS NULL
			) ON CONFLICT(Id) DO NOTHING`, id, cameraID, transition.EventType,
				formatTime(&transition.OccurredAtUtc), string(transition.RawPayload), formatTime(&transition.OccurredAtUtc),
				cameraID, transition.EventType)
			if err != nil {
				return fmt.Errorf("insert event: %w", err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE Events SET EndedAtUtc = ?
				WHERE CameraId = ? AND EndedAtUtc IS NULL AND StartedAtUtc <= ?`,
				formatTime(&transition.OccurredAtUtc), cameraID, formatTime(&transition.OccurredAtUtc)); err != nil {
				return fmt.Errorf("close events: %w", err)
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE Cameras SET LastEventAtUtc = CASE
			WHEN LastEventAtUtc IS NULL OR LastEventAtUtc < ? THEN ? ELSE LastEventAtUtc END
			WHERE Id = ? AND RetiredAtUtc IS NULL`, formatTime(&transition.OccurredAtUtc),
			formatTime(&transition.OccurredAtUtc), cameraID)
		if err != nil {
			return fmt.Errorf("update camera event time: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read camera event update count: %w", err)
		}
		if count == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (store *Store) ListEvents(ctx context.Context, cameraID string, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := store.db.QueryContext(ctx, `SELECT Id, CameraId, EventType, StartedAtUtc,
		EndedAtUtc, NativeRecordingName, RawPayloadJson, CreatedAtUtc
		FROM Events WHERE CameraId = ? ORDER BY StartedAtUtc DESC LIMIT ?`, cameraID, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (store *Store) ListArchivedEvents(ctx context.Context, cameraID string, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := store.db.QueryContext(ctx, `SELECT Events.Id, Events.CameraId, Events.EventType,
		Events.StartedAtUtc, Events.EndedAtUtc, Events.NativeRecordingName,
		Events.RawPayloadJson, Events.CreatedAtUtc
		FROM Events WHERE Events.CameraId = ? AND EXISTS (
			SELECT 1 FROM Recordings WHERE Recordings.CameraId = Events.CameraId
			AND Recordings.State = 'Complete' AND Recordings.LocalPath IS NOT NULL AND Recordings.LocalPath != ''
			AND COALESCE(json_extract(CASE WHEN json_valid(Recordings.RawMetadataJson) THEN Recordings.RawMetadataJson ELSE '{}' END,
			'$._reonvrEstimatedClock'), 0) != 1
			AND COALESCE(json_extract(CASE WHEN json_valid(Recordings.RawMetadataJson) THEN Recordings.RawMetadataJson ELSE '{}' END,
			'$._reonvrSidecarPending'), 0) != 1
			AND EXISTS (SELECT 1 FROM RecordingCoverage Coverage WHERE Coverage.RecordingId = Recordings.Id
				AND julianday(Coverage.StartAtUtc) <= julianday(COALESCE(Events.EndedAtUtc, Events.StartedAtUtc))
				AND julianday(Coverage.EndAtUtc) > julianday(Events.StartedAtUtc))
			AND (Recordings.NativeName = Events.NativeRecordingName OR (
				Recordings.Source IN ('Rtsp', 'CameraStorage')
				AND julianday(Recordings.StartAtUtc) <= julianday(COALESCE(Events.EndedAtUtc, Events.StartedAtUtc))
				AND julianday(Recordings.EndAtUtc) >= julianday(Events.StartedAtUtc)
			))
		) ORDER BY Events.StartedAtUtc DESC LIMIT ?`, cameraID, limit)
	if err != nil {
		return nil, fmt.Errorf("list archived events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]domain.Event, error) {
	events := make([]domain.Event, 0)
	for rows.Next() {
		var event domain.Event
		var started, created string
		var ended, nativeName sql.NullString
		var rawPayload string
		if err := rows.Scan(&event.ID, &event.CameraID, &event.EventType, &started,
			&ended, &nativeName, &rawPayload, &created); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		var parseErr error
		if event.StartedAtUtc, parseErr = time.Parse(time.RFC3339Nano, started); parseErr != nil {
			return nil, fmt.Errorf("parse event start: %w", parseErr)
		}
		if ended.Valid {
			parsed, parseErr := time.Parse(time.RFC3339Nano, ended.String)
			if parseErr != nil {
				return nil, fmt.Errorf("parse event end: %w", parseErr)
			}
			event.EndedAtUtc = &parsed
		}
		if event.CreatedAtUtc, parseErr = time.Parse(time.RFC3339Nano, created); parseErr != nil {
			return nil, fmt.Errorf("parse event creation: %w", parseErr)
		}
		event.NativeRecordingName = nativeName.String
		event.RawPayload = json.RawMessage(rawPayload)
		events = append(events, event)
	}
	return events, rows.Err()
}

func validEventType(eventType domain.EventType) bool {
	switch eventType {
	case domain.EventMotion, domain.EventPerson, domain.EventVehicle, domain.EventAnimal:
		return true
	default:
		return false
	}
}

func (store *Store) RecoverRecordingWork(ctx context.Context, now time.Time) error {
	_, err := store.db.ExecContext(ctx, `UPDATE Recordings SET State = 'RetryWait', UpdatedAtUtc = ?
		WHERE State IN ('Downloading', 'Verifying', 'Recording')`, formatTime(&now))
	if err != nil {
		return fmt.Errorf("recover recording work: %w", err)
	}
	return nil
}

func scanRecording(row rowScanner) (domain.Recording, error) {
	var recording domain.Recording
	var start, end, rawMetadata, discovered, updated string
	if err := row.Scan(&recording.ID, &recording.CameraID, &recording.NativeName,
		&recording.Source, &recording.Stream, &recording.Codec, &start, &end,
		&recording.SizeBytes, &recording.State, &rawMetadata, &discovered, &updated,
		&recording.LocalPath, &recording.Sha256); err != nil {
		return domain.Recording{}, err
	}
	recording.RawMetadata = json.RawMessage(rawMetadata)
	var err error
	if recording.StartAtUtc, err = time.Parse(time.RFC3339Nano, start); err != nil {
		return domain.Recording{}, fmt.Errorf("parse recording start: %w", err)
	}
	if recording.EndAtUtc, err = time.Parse(time.RFC3339Nano, end); err != nil {
		return domain.Recording{}, fmt.Errorf("parse recording end: %w", err)
	}
	if recording.DiscoveredAtUtc, err = time.Parse(time.RFC3339Nano, discovered); err != nil {
		return domain.Recording{}, fmt.Errorf("parse recording discovery time: %w", err)
	}
	if recording.UpdatedAtUtc, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return domain.Recording{}, fmt.Errorf("parse recording update time: %w", err)
	}
	return recording, nil
}

func (store *Store) listRecordingTags(ctx context.Context, recordingID string) ([]domain.EventType, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT EventType FROM RecordingTags WHERE RecordingId = ? ORDER BY EventType`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("list recording tags: %w", err)
	}
	defer rows.Close()
	tags := make([]domain.EventType, 0)
	for rows.Next() {
		var eventType domain.EventType
		if err := rows.Scan(&eventType); err != nil {
			return nil, fmt.Errorf("scan recording tag: %w", err)
		}
		tags = append(tags, eventType)
	}
	return tags, rows.Err()
}

func (store *Store) SaveWorkerState(ctx context.Context, state domain.WorkerState) error {
	_, err := store.db.ExecContext(ctx, `INSERT INTO WorkerStates (
		WorkerName, ScopeKey, Stage, LastHeartbeatAtUtc, LastSuccessAtUtc,
		ConsecutiveFailures, NextAttemptAtUtc, LastErrorCode, UpdatedAtUtc
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(WorkerName, ScopeKey) DO UPDATE SET Stage = excluded.Stage,
	LastHeartbeatAtUtc = excluded.LastHeartbeatAtUtc, LastSuccessAtUtc = excluded.LastSuccessAtUtc,
	ConsecutiveFailures = excluded.ConsecutiveFailures, NextAttemptAtUtc = excluded.NextAttemptAtUtc,
	LastErrorCode = excluded.LastErrorCode, UpdatedAtUtc = excluded.UpdatedAtUtc`,
		state.WorkerName, state.ScopeKey, state.Stage, formatTime(state.LastHeartbeatAtUtc),
		formatTime(state.LastSuccessAtUtc), state.ConsecutiveFailures, formatTime(state.NextAttemptAtUtc),
		nullableString(state.LastErrorCode), formatTime(&state.UpdatedAtUtc))
	if err != nil {
		return fmt.Errorf("save worker state: %w", err)
	}
	return nil
}

func (store *Store) ReportIssue(ctx context.Context, issue domain.OperationalIssue) error {
	_, err := store.db.ExecContext(ctx, `INSERT INTO OperationalIssues (
		Id, Severity, Source, CameraId, ErrorCode, Message, FirstSeenAtUtc,
		LastSeenAtUtc, OccurrenceCount, ResolvedAtUtc
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, NULL)
	ON CONFLICT DO UPDATE SET Severity = excluded.Severity, Message = excluded.Message,
	LastSeenAtUtc = excluded.LastSeenAtUtc, OccurrenceCount = OperationalIssues.OccurrenceCount + 1,
	ResolvedAtUtc = NULL`,
		issue.ID, issue.Severity, issue.Source, nullableString(issue.CameraID), issue.ErrorCode,
		issue.Message, formatTime(&issue.FirstSeenAtUtc), formatTime(&issue.LastSeenAtUtc))
	if err != nil {
		return fmt.Errorf("report operational issue: %w", err)
	}
	return nil
}

func (store *Store) ResolveIssue(ctx context.Context, source, cameraID, errorCode string, now time.Time) error {
	_, err := store.db.ExecContext(ctx, `UPDATE OperationalIssues SET ResolvedAtUtc = ?, LastSeenAtUtc = ? WHERE Source = ? AND IFNULL(CameraId, '') = ? AND ErrorCode = ? AND ResolvedAtUtc IS NULL`,
		formatTime(&now), formatTime(&now), source, cameraID, errorCode)
	if err != nil {
		return fmt.Errorf("resolve operational issue: %w", err)
	}
	return nil
}

func (store *Store) ListOperationalIssues(ctx context.Context) ([]domain.OperationalIssue, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT Id, Severity, Source, IFNULL(CameraId, ''), ErrorCode, Message, FirstSeenAtUtc, LastSeenAtUtc, OccurrenceCount, ResolvedAtUtc FROM OperationalIssues ORDER BY LastSeenAtUtc DESC`)
	if err != nil {
		return nil, fmt.Errorf("list operational issues: %w", err)
	}
	defer rows.Close()
	issues := make([]domain.OperationalIssue, 0)
	for rows.Next() {
		var issue domain.OperationalIssue
		var firstSeen, lastSeen string
		var resolved sql.NullString
		if err := rows.Scan(&issue.ID, &issue.Severity, &issue.Source, &issue.CameraID, &issue.ErrorCode, &issue.Message, &firstSeen, &lastSeen, &issue.OccurrenceCount, &resolved); err != nil {
			return nil, err
		}
		issue.FirstSeenAtUtc, err = time.Parse(time.RFC3339Nano, firstSeen)
		if err != nil {
			return nil, err
		}
		issue.LastSeenAtUtc, err = time.Parse(time.RFC3339Nano, lastSeen)
		if err != nil {
			return nil, err
		}
		if resolved.Valid {
			parsed, err := time.Parse(time.RFC3339Nano, resolved.String)
			if err != nil {
				return nil, err
			}
			issue.ResolvedAtUtc = &parsed
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

func (store *Store) Maintain(ctx context.Context) error {
	var result string
	if err := store.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("check database integrity: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("database integrity check returned %s", result)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		return fmt.Errorf("checkpoint database: %w", err)
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func applyCameraPolicyDefaults(camera *domain.Camera) {
	if camera.RecordingSource == "" {
		camera.RecordingSource = domain.RecordingSourceCameraStorage
	}
	if camera.RecordingMode == "" {
		camera.RecordingMode = domain.RecordingModeEvents
	}
	if camera.RecordingSource == domain.RecordingSourceCameraStorage {
		camera.HistoricalImport = domain.HistoricalImportAll
		camera.ArchiveFromUtc = nil
	} else if camera.HistoricalImport == "" {
		camera.HistoricalImport = domain.HistoricalImportNewOnly
	}
	if camera.ArchiveFromUtc == nil && camera.HistoricalImport != domain.HistoricalImportAll {
		value := camera.UpdatedAtUtc.UTC()
		camera.ArchiveFromUtc = &value
	}
}
