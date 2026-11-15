package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reonavr/internal/domain"
	"reonavr/internal/reolink"
)

func (store *Store) migrateRecovery(ctx context.Context) error {
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SchemaMigrations WHERE Version = 8`).Scan(&applied); err != nil || applied != 0 {
		return err
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, statement := range []string{
			`ALTER TABLE Cameras ADD COLUMN RecoveryEnabled INTEGER NOT NULL DEFAULT 0`,
			`CREATE TABLE RecoveryPeriods (
				Id TEXT PRIMARY KEY, CameraId TEXT NOT NULL REFERENCES Cameras(Id), Stream TEXT NOT NULL,
				Mode TEXT NOT NULL, EventTypesJson TEXT NOT NULL, PreEventSeconds INTEGER NOT NULL,
				PostEventSeconds INTEGER NOT NULL, StartedAtUtc TEXT NOT NULL, EndedAtUtc TEXT, ScannedUntilUtc TEXT NOT NULL)`,
			`CREATE UNIQUE INDEX IndexRecoveryPeriodsOpen ON RecoveryPeriods(CameraId) WHERE EndedAtUtc IS NULL`,
			`CREATE TABLE RecordingCoverage (
				RecordingId TEXT NOT NULL REFERENCES Recordings(Id) ON DELETE CASCADE,
				StartAtUtc TEXT NOT NULL, EndAtUtc TEXT NOT NULL, PRIMARY KEY(RecordingId, StartAtUtc))`,
			`CREATE TABLE CaptureCoverage (
				CameraId TEXT NOT NULL REFERENCES Cameras(Id), Stream TEXT NOT NULL,
				StartAtUtc TEXT NOT NULL, EndAtUtc TEXT NOT NULL, PRIMARY KEY(CameraId, Stream, StartAtUtc))`,
			`CREATE TABLE RecoveryGaps (
				Id TEXT PRIMARY KEY, PeriodId TEXT NOT NULL REFERENCES RecoveryPeriods(Id),
				StartAtUtc TEXT NOT NULL, EndAtUtc TEXT NOT NULL, Status TEXT NOT NULL DEFAULT 'Pending',
				MissingSeconds REAL NOT NULL, LastCheckedAtUtc TEXT, UNIQUE(PeriodId, StartAtUtc, EndAtUtc))`,
			`CREATE TABLE RecoveryGapRecordings (
				GapId TEXT NOT NULL REFERENCES RecoveryGaps(Id), RecordingId TEXT NOT NULL REFERENCES Recordings(Id),
				PRIMARY KEY(GapId, RecordingId))`,
			`UPDATE Cameras SET RecoveryEnabled = CASE WHEN RecordingSource = 'CameraStorage' THEN 1 ELSE 0 END,
				RecordingSource = 'Rtsp', HistoricalImport = 'NewOnly', ArchiveFromUtc = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
			`UPDATE Recordings SET State = 'Discovered' WHERE Source = 'CameraStorage'
				AND State IN ('Queued', 'Downloading', 'Verifying', 'RetryWait')`,
			`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (8, CURRENT_TIMESTAMP)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate recovery: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) migrateRecoveryRetries(ctx context.Context) error {
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SchemaMigrations WHERE Version = 9`).Scan(&applied); err != nil || applied != 0 {
		return err
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, statement := range []string{
			`ALTER TABLE RecoveryGaps ADD COLUMN SearchTimezone TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE RecoveryGaps ADD COLUMN SearchCursorUtc TEXT`,
			`ALTER TABLE RecoveryGaps ADD COLUMN NextSearchAtUtc TEXT`,
			`ALTER TABLE Recordings ADD COLUMN SidecarRepairAttemptAtUtc TEXT`,
			`INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (9, CURRENT_TIMESTAMP)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}

func (store *Store) migrateRecoveryAmbiguity(ctx context.Context) error {
	var applied int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM SchemaMigrations WHERE Version = 10`).Scan(&applied); err != nil || applied != 0 {
		return err
	}
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE RecoveryGaps ADD COLUMN ClockAmbiguous INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO SchemaMigrations (Version, AppliedAtUtc) VALUES (10, CURRENT_TIMESTAMP)`)
		return err
	})
}

func (store *Store) SyncRecordingIntent(ctx context.Context, camera domain.Camera, enabled bool, now time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		var recordingEnabled bool
		if err := tx.QueryRowContext(ctx, `SELECT RecordingEnabled FROM ApplicationSettings WHERE Id = 1`).Scan(&recordingEnabled); err != nil {
			return err
		}
		selected, err := json.Marshal(camera.EventTypes)
		if err != nil {
			return err
		}
		var periodID, stream, mode, eventTypes string
		var pre, post int
		err = tx.QueryRowContext(ctx, `SELECT Id, Stream, Mode, EventTypesJson, PreEventSeconds, PostEventSeconds
			FROM RecoveryPeriods WHERE CameraId = ? AND EndedAtUtc IS NULL`, camera.ID).Scan(&periodID, &stream, &mode, &eventTypes, &pre, &post)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		active := enabled && recordingEnabled && camera.Enabled && camera.ArchiveEnabled && camera.RecoveryEnabled
		if err == nil && active && stream == string(camera.ArchiveStream) && mode == string(camera.RecordingMode) &&
			eventTypes == string(selected) && pre == camera.PreEventSeconds && post == camera.PostEventSeconds {
			return nil
		}
		if err == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE RecoveryPeriods SET EndedAtUtc = ? WHERE Id = ?`, formatTime(&now), periodID); err != nil {
				return err
			}
		}
		if !active {
			return nil
		}
		periodID = fmt.Sprintf("%s-%d", camera.ID, now.UnixNano())
		_, err = tx.ExecContext(ctx, `INSERT INTO RecoveryPeriods
			(Id, CameraId, Stream, Mode, EventTypesJson, PreEventSeconds, PostEventSeconds, StartedAtUtc, ScannedUntilUtc)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, periodID, camera.ID, camera.ArchiveStream, camera.RecordingMode,
			string(selected), camera.PreEventSeconds, camera.PostEventSeconds, formatTime(&now), formatTime(&now))
		return err
	})
}

func (store *Store) CloseRecordingIntents(ctx context.Context, cameraID string, now time.Time) error {
	_, err := store.db.ExecContext(ctx, `UPDATE RecoveryPeriods SET EndedAtUtc = ? WHERE EndedAtUtc IS NULL AND (? = '' OR CameraId = ?)`, formatTime(&now), cameraID, cameraID)
	return err
}

func (store *Store) ListRecoveryPeriods(ctx context.Context, cameraID string, before time.Time) ([]domain.RecoveryPeriod, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT Id, Stream, Mode, EventTypesJson, PreEventSeconds, PostEventSeconds,
		StartedAtUtc, EndedAtUtc, ScannedUntilUtc FROM RecoveryPeriods
		WHERE CameraId = ? AND julianday(ScannedUntilUtc) < julianday(?)
		AND (EndedAtUtc IS NULL OR julianday(ScannedUntilUtc) < julianday(EndedAtUtc)) ORDER BY StartedAtUtc LIMIT 4`, cameraID, formatTime(&before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var periods []domain.RecoveryPeriod
	for rows.Next() {
		period := domain.RecoveryPeriod{CameraID: cameraID}
		var events, start, cursor string
		var end sql.NullString
		if err := rows.Scan(&period.ID, &period.Stream, &period.Mode, &events, &period.PreEventSeconds, &period.PostEventSeconds, &start, &end, &cursor); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(events), &period.EventTypes); err != nil {
			return nil, err
		}
		if period.StartAtUtc, err = time.Parse(time.RFC3339Nano, start); err != nil {
			return nil, err
		}
		if period.ScannedUntilUtc, err = time.Parse(time.RFC3339Nano, cursor); err != nil {
			return nil, err
		}
		if end.Valid {
			value, parseErr := time.Parse(time.RFC3339Nano, end.String)
			if parseErr != nil {
				return nil, parseErr
			}
			period.EndAtUtc = &value
		}
		periods = append(periods, period)
	}
	return periods, rows.Err()
}

func (store *Store) SaveRecoveryScan(ctx context.Context, periodID string, until time.Time, gaps []domain.RecoveryGap) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, gap := range gaps {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO RecoveryGaps (Id, PeriodId, StartAtUtc, EndAtUtc, MissingSeconds)
				VALUES (?, ?, ?, ?, ?)`, gap.ID, periodID, formatTime(&gap.StartAtUtc), formatTime(&gap.EndAtUtc), gap.EndAtUtc.Sub(gap.StartAtUtc).Seconds()); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE RecoveryPeriods SET ScannedUntilUtc = ? WHERE Id = ?`, formatTime(&until), periodID)
		return err
	})
}

func (store *Store) ListSidecarRepairs(ctx context.Context, limit int) ([]domain.Recording, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT Id FROM Recordings WHERE State = 'Complete' AND COALESCE(LocalPath, '') != ''
		AND COALESCE(json_extract(CASE WHEN json_valid(RawMetadataJson) THEN RawMetadataJson ELSE '{}' END,
		'$._reonvrSidecarPending'), 0) = 1 ORDER BY SidecarRepairAttemptAtUtc, UpdatedAtUtc LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var identifiers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		identifiers = append(identifiers, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var recordings []domain.Recording
	for _, id := range identifiers {
		recording, err := store.GetRecording(ctx, id)
		if err != nil {
			return nil, err
		}
		recordings = append(recordings, recording)
	}
	return recordings, nil
}

func (store *Store) DeferSidecarRepair(ctx context.Context, id string, now time.Time) error {
	_, err := store.db.ExecContext(ctx, `UPDATE Recordings SET SidecarRepairAttemptAtUtc = ? WHERE Id = ? AND State = 'Complete'
		AND COALESCE(json_extract(CASE WHEN json_valid(RawMetadataJson) THEN RawMetadataJson ELSE '{}' END,
		'$._reonvrSidecarPending'), 0) = 1`, formatTime(&now), id)
	return err
}

func (store *Store) CompleteSidecarRepair(ctx context.Context, recording domain.Recording, coverage []domain.TimeRange) (bool, error) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(recording.RawMetadata, &metadata); err != nil {
		return false, err
	}
	coveragePending := string(metadata["_reonvrCoveragePending"]) == "true"
	delete(metadata, "_reonvrSidecarPending")
	delete(metadata, "_reonvrCoveragePending")
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return false, err
	}
	completed := false
	err = store.withTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE Recordings SET RawMetadataJson = ? WHERE Id = ? AND State = 'Complete'
		AND StartAtUtc = ? AND EndAtUtc = ? AND RawMetadataJson = ? AND LocalPath = ? AND Sha256 = ?`,
			string(encoded), recording.ID, formatTime(&recording.StartAtUtc), formatTime(&recording.EndAtUtc), string(recording.RawMetadata), recording.LocalPath, recording.Sha256)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		completed = count == 1
		if completed && coveragePending && coverage == nil {
			return errors.New("corrected coverage requires retained-media reinspection")
		}
		if completed && coverage != nil {
			return saveRecordingCoverageTx(ctx, tx, recording.ID, coverage)
		}
		return nil
	})
	return completed && err == nil, err
}

func (store *Store) SaveRecordingCoverage(ctx context.Context, recordingID string, ranges []domain.TimeRange) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		return saveRecordingCoverageTx(ctx, tx, recordingID, ranges)
	})
}

func saveRecordingCoverageTx(ctx context.Context, tx *sql.Tx, recordingID string, ranges []domain.TimeRange) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM RecordingCoverage WHERE RecordingId = ?`, recordingID); err != nil {
		return err
	}
	for _, span := range ranges {
		if !span.StartAtUtc.Before(span.EndAtUtc) {
			return errors.New("invalid verified coverage")
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO RecordingCoverage (RecordingId, StartAtUtc, EndAtUtc) VALUES (?, ?, ?)`, recordingID, formatTime(&span.StartAtUtc), formatTime(&span.EndAtUtc)); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) SaveCaptureCoverage(ctx context.Context, cameraID string, stream domain.ArchiveStream, ranges []domain.TimeRange) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		for _, span := range ranges {
			from, to := span.StartAtUtc, span.EndAtUtc
			left, right := from.Add(-2*time.Second), to.Add(2*time.Second)
			var existingStart, existingEnd sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT MIN(StartAtUtc), MAX(EndAtUtc) FROM CaptureCoverage
				WHERE CameraId = ? AND Stream = ? AND julianday(EndAtUtc) >= julianday(?) AND julianday(StartAtUtc) <= julianday(?)`, cameraID, stream, formatTime(&left), formatTime(&right)).Scan(&existingStart, &existingEnd); err != nil {
				return err
			}
			if existingStart.Valid {
				start, err := time.Parse(time.RFC3339Nano, existingStart.String)
				if err != nil {
					return err
				}
				end, err := time.Parse(time.RFC3339Nano, existingEnd.String)
				if err != nil {
					return err
				}
				if start.Before(from) {
					from = start
				}
				if end.After(to) {
					to = end
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM CaptureCoverage WHERE CameraId = ? AND Stream = ? AND julianday(EndAtUtc) >= julianday(?) AND julianday(StartAtUtc) <= julianday(?)`, cameraID, stream, formatTime(&left), formatTime(&right)); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO CaptureCoverage (CameraId, Stream, StartAtUtc, EndAtUtc) VALUES (?, ?, ?, ?)`, cameraID, stream, formatTime(&from), formatTime(&to)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (store *Store) ListVerifiedCoverage(ctx context.Context, cameraID string, stream domain.ArchiveStream, span domain.TimeRange, capture bool) ([]domain.TimeRange, error) {
	query := `SELECT Coverage.StartAtUtc, Coverage.EndAtUtc FROM RecordingCoverage Coverage JOIN Recordings ON Recordings.Id = Coverage.RecordingId
		WHERE Recordings.CameraId = ? AND Recordings.Stream = ? AND Recordings.State IN ('Complete', 'Deleted')
		AND COALESCE(json_extract(CASE WHEN json_valid(Recordings.RawMetadataJson) THEN Recordings.RawMetadataJson ELSE '{}' END,
		'$._reonvrEstimatedClock'), 0) != 1
		AND COALESCE(json_extract(CASE WHEN json_valid(Recordings.RawMetadataJson) THEN Recordings.RawMetadataJson ELSE '{}' END,
		'$._reonvrSidecarPending'), 0) != 1
		AND COALESCE(json_extract(CASE WHEN json_valid(Recordings.RawMetadataJson) THEN Recordings.RawMetadataJson ELSE '{}' END,
		'$._reonvrCoveragePending'), 0) != 1`
	if capture {
		query = `SELECT Coverage.StartAtUtc, Coverage.EndAtUtc FROM CaptureCoverage Coverage WHERE Coverage.CameraId = ? AND Coverage.Stream = ?`
	}
	rows, err := store.db.QueryContext(ctx, query+` AND julianday(Coverage.EndAtUtc) > julianday(?) AND julianday(Coverage.StartAtUtc) < julianday(?)`, cameraID, stream, formatTime(&span.StartAtUtc), formatTime(&span.EndAtUtc))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ranges []domain.TimeRange
	for rows.Next() {
		var start, end string
		if err := rows.Scan(&start, &end); err != nil {
			return nil, err
		}
		from, err := time.Parse(time.RFC3339Nano, start)
		if err != nil {
			return nil, err
		}
		to, err := time.Parse(time.RFC3339Nano, end)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, domain.TimeRange{StartAtUtc: from, EndAtUtc: to})
	}
	return ranges, rows.Err()
}

func (store *Store) ListRequiredEventCoverage(ctx context.Context, gap domain.RecoveryGap) ([]domain.TimeRange, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT Events.StartedAtUtc, Events.EndedAtUtc, Events.EventType,
		Periods.PreEventSeconds, Periods.PostEventSeconds FROM Events
		JOIN RecoveryPeriods Periods ON Periods.Id = ? AND Periods.CameraId = Events.CameraId
		WHERE Events.CameraId = ?
		AND julianday(Events.StartedAtUtc) <= julianday(?) + Periods.PreEventSeconds / 86400.0
		AND (Events.EndedAtUtc IS NULL OR julianday(Events.EndedAtUtc) >= julianday(?) - Periods.PostEventSeconds / 86400.0)
		UNION ALL SELECT Recordings.StartAtUtc, Recordings.EndAtUtc, NULL, 0, 0
		FROM RecoveryGapRecordings Links JOIN Recordings ON Recordings.Id = Links.RecordingId
		WHERE Links.GapId = ? AND Recordings.CameraId = ? AND Recordings.Stream = ?`,
		gap.PeriodID, gap.CameraID, formatTime(&gap.EndAtUtc), formatTime(&gap.StartAtUtc), gap.ID, gap.CameraID, gap.Stream)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	selected := make(map[domain.EventType]bool)
	for _, eventType := range gap.EventTypes {
		selected[eventType] = true
	}
	var coverage []domain.TimeRange
	for rows.Next() {
		var start string
		var end sql.NullString
		var eventType sql.NullString
		var pre, post int
		if err := rows.Scan(&start, &end, &eventType, &pre, &post); err != nil {
			return nil, err
		}
		if eventType.Valid && !selected[domain.EventType(eventType.String)] {
			continue
		}
		from, err := time.Parse(time.RFC3339Nano, start)
		if err != nil {
			return nil, err
		}
		from = from.Add(-time.Duration(pre) * time.Second)
		to := gap.EndAtUtc
		if end.Valid {
			to, err = time.Parse(time.RFC3339Nano, end.String)
			if err != nil {
				return nil, err
			}
			to = to.Add(time.Duration(post) * time.Second)
		}
		if from.Before(gap.StartAtUtc) {
			from = gap.StartAtUtc
		}
		if to.After(gap.EndAtUtc) {
			to = gap.EndAtUtc
		}
		if from.Before(to) {
			coverage = append(coverage, domain.TimeRange{StartAtUtc: from, EndAtUtc: to})
		}
	}
	return coverage, rows.Err()
}

func (store *Store) ListRecoveryGaps(ctx context.Context, cameraID string, limit int) ([]domain.RecoveryGap, error) {
	return store.listRecoveryGaps(ctx, cameraID, limit, false, nil)
}

func (store *Store) ListRecoveryWork(ctx context.Context, cameraID string, limit int, now time.Time) ([]domain.RecoveryGap, error) {
	return store.listRecoveryGaps(ctx, cameraID, limit, false, &now)
}

func (store *Store) ListRecoveryHistory(ctx context.Context, cameraID string, limit int) ([]domain.RecoveryGap, error) {
	return store.listRecoveryGaps(ctx, cameraID, limit, true, nil)
}

func (store *Store) listRecoveryGaps(ctx context.Context, cameraID string, limit int, history bool, now *time.Time) ([]domain.RecoveryGap, error) {
	query := `SELECT Gaps.Id, Gaps.PeriodId, Periods.Stream, Periods.Mode, Periods.EventTypesJson,
		Gaps.StartAtUtc, Gaps.EndAtUtc, Gaps.Status, Gaps.MissingSeconds, Gaps.LastCheckedAtUtc,
		Gaps.SearchTimezone, Gaps.SearchCursorUtc, Gaps.NextSearchAtUtc, Gaps.ClockAmbiguous
		FROM RecoveryGaps Gaps JOIN RecoveryPeriods Periods ON Periods.Id = Gaps.PeriodId
		WHERE Periods.CameraId = ?`
	arguments := []any{cameraID}
	if history {
		query += ` ORDER BY CASE WHEN Gaps.Status IN ('Recovered', 'RecoveredEvents', 'CheckedEvents') THEN 1 ELSE 0 END,
			Gaps.StartAtUtc DESC LIMIT ?`
	} else {
		query += ` AND Gaps.Status NOT IN ('Recovered', 'RecoveredEvents', 'CheckedEvents') ORDER BY `
		if now != nil {
			query += `CASE WHEN julianday(Gaps.NextSearchAtUtc) > julianday(?) AND
				Gaps.SearchTimezone = (SELECT Timezone FROM Cameras WHERE Id = Periods.CameraId) THEN 1 ELSE 0 END, `
			arguments = append(arguments, formatTime(now))
		}
		query += `Gaps.LastCheckedAtUtc, Gaps.StartAtUtc LIMIT ?`
	}
	arguments = append(arguments, limit)
	rows, err := store.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	gaps := make([]domain.RecoveryGap, 0)
	for rows.Next() {
		gap := domain.RecoveryGap{CameraID: cameraID}
		var events, start, end string
		var checked, cursor, next sql.NullString
		if err := rows.Scan(&gap.ID, &gap.PeriodID, &gap.Stream, &gap.Mode, &events, &start, &end, &gap.Status, &gap.MissingSeconds, &checked, &gap.SearchTimezone, &cursor, &next, &gap.ClockAmbiguous); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(events), &gap.EventTypes); err != nil {
			return nil, err
		}
		if gap.StartAtUtc, err = time.Parse(time.RFC3339Nano, start); err != nil {
			return nil, err
		}
		if gap.EndAtUtc, err = time.Parse(time.RFC3339Nano, end); err != nil {
			return nil, err
		}
		if checked.Valid {
			value, parseErr := time.Parse(time.RFC3339Nano, checked.String)
			if parseErr != nil {
				return nil, parseErr
			}
			gap.LastCheckedAtUtc = &value
		}
		if cursor.Valid {
			value, parseErr := time.Parse(time.RFC3339Nano, cursor.String)
			if parseErr != nil {
				return nil, parseErr
			}
			gap.SearchCursorUtc = &value
		}
		if next.Valid {
			value, parseErr := time.Parse(time.RFC3339Nano, next.String)
			if parseErr != nil {
				return nil, parseErr
			}
			gap.NextSearchAtUtc = &value
		}
		gaps = append(gaps, gap)
	}
	return gaps, rows.Err()
}

func (store *Store) PrepareRecoveryGap(ctx context.Context, gap domain.RecoveryGap, timezone string) (domain.RecoveryGap, bool, error) {
	active := false
	err := store.withTransaction(ctx, func(tx *sql.Tx) error {
		var start, end string
		var cursor, next sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT StartAtUtc, EndAtUtc, Status, SearchTimezone, SearchCursorUtc, NextSearchAtUtc, ClockAmbiguous
			FROM RecoveryGaps WHERE Id = ?`, gap.ID).Scan(&start, &end, &gap.Status, &gap.SearchTimezone, &cursor, &next, &gap.ClockAmbiguous)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if gap.Status == "Recovered" || gap.Status == "RecoveredEvents" || gap.Status == "CheckedEvents" {
			return nil
		}
		gap.StartAtUtc, err = time.Parse(time.RFC3339Nano, start)
		if err != nil {
			return err
		}
		gap.EndAtUtc, err = time.Parse(time.RFC3339Nano, end)
		if err != nil {
			return err
		}
		gap.SearchCursorUtc, gap.NextSearchAtUtc = nil, nil
		if gap.SearchTimezone != timezone {
			gap.SearchTimezone = timezone
			gap.ClockAmbiguous = false
			if _, err := tx.ExecContext(ctx, `UPDATE RecoveryGaps SET SearchTimezone = ?, SearchCursorUtc = NULL, NextSearchAtUtc = NULL, ClockAmbiguous = 0 WHERE Id = ?`, timezone, gap.ID); err != nil {
				return err
			}
		} else {
			if cursor.Valid {
				value, err := time.Parse(time.RFC3339Nano, cursor.String)
				if err != nil {
					return err
				}
				gap.SearchCursorUtc = &value
			}
			if next.Valid {
				value, err := time.Parse(time.RFC3339Nano, next.String)
				if err != nil {
					return err
				}
				gap.NextSearchAtUtc = &value
			}
		}
		if !reolink.CameraClockEstimated(timezone) && !gap.ClockAmbiguous && gap.EndAtUtc.Sub(gap.StartAtUtc) > 30*time.Minute {
			tailStart := gap.StartAtUtc.Add(30 * time.Minute)
			tailID := fmt.Sprintf("%x", sha256.Sum256([]byte(gap.ID+tailStart.Format(time.RFC3339Nano))))
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO RecoveryGaps (Id, PeriodId, StartAtUtc, EndAtUtc, Status, MissingSeconds, SearchTimezone)
				VALUES (?, ?, ?, ?, 'Pending', ?, ?)`, tailID, gap.PeriodID, formatTime(&tailStart), formatTime(&gap.EndAtUtc), gap.EndAtUtc.Sub(tailStart).Seconds(), timezone); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT Id FROM RecoveryGaps WHERE PeriodId = ? AND StartAtUtc = ? AND EndAtUtc = ?`,
				gap.PeriodID, formatTime(&tailStart), formatTime(&gap.EndAtUtc)).Scan(&tailID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO RecoveryGapRecordings (GapId, RecordingId)
				SELECT ?, RecordingId FROM RecoveryGapRecordings WHERE GapId = ?`, tailID, gap.ID); err != nil {
				return err
			}
			gap.EndAtUtc = tailStart
			var headID string
			headErr := tx.QueryRowContext(ctx, `SELECT Id FROM RecoveryGaps WHERE PeriodId = ? AND StartAtUtc = ? AND EndAtUtc = ? AND Id != ?`,
				gap.PeriodID, formatTime(&gap.StartAtUtc), formatTime(&gap.EndAtUtc), gap.ID).Scan(&headID)
			if headErr == nil {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO RecoveryGapRecordings (GapId, RecordingId) SELECT ?, RecordingId FROM RecoveryGapRecordings WHERE GapId = ?`, headID, gap.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM RecoveryGapRecordings WHERE GapId = ?`, gap.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM RecoveryGaps WHERE Id = ?`, gap.ID); err != nil {
					return err
				}
				gap.ID, gap.Status = headID, "Pending"
			} else if !errors.Is(headErr, sql.ErrNoRows) {
				return headErr
			}
			if _, err := tx.ExecContext(ctx, `UPDATE RecoveryGaps SET EndAtUtc = ?, Status = 'Pending', SearchTimezone = ?, SearchCursorUtc = NULL, NextSearchAtUtc = NULL, ClockAmbiguous = 0 WHERE Id = ?`, formatTime(&tailStart), timezone, gap.ID); err != nil {
				return err
			}
			gap.SearchCursorUtc, gap.NextSearchAtUtc = nil, nil
		}
		active = true
		return nil
	})
	return gap, active, err
}

func (store *Store) UpdateProvisionalRecoveryGap(ctx context.Context, gap domain.RecoveryGap, searchedUntil time.Time, missing float64, now time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		return updateProvisionalRecoveryGapTx(ctx, tx, gap, searchedUntil, missing, now)
	})
}

func updateProvisionalRecoveryGapTx(ctx context.Context, tx *sql.Tx, gap domain.RecoveryGap, searchedUntil time.Time, missing float64, now time.Time) error {
	next := now.Add(10 * time.Minute)
	var start, end string
	if err := tx.QueryRowContext(ctx, `SELECT StartAtUtc, EndAtUtc FROM RecoveryGaps WHERE Id = ?`, gap.ID).Scan(&start, &end); err != nil {
		return err
	}
	limit, err := time.Parse(time.RFC3339Nano, end)
	if err != nil {
		return err
	}
	if gap.ClockAmbiguous && !searchedUntil.Before(limit) {
		next = now.Add(24 * time.Hour)
	}
	cursor := formatTime(&searchedUntil)
	if !searchedUntil.Before(limit) {
		cursor = start
	}
	if _, err := tx.ExecContext(ctx, `UPDATE RecoveryGaps SET Status = 'Provisional', MissingSeconds = ?, LastCheckedAtUtc = ?, SearchCursorUtc = ?, NextSearchAtUtc = ?, ClockAmbiguous = ?
			WHERE Id = ?`, missing, formatTime(&now), cursor, formatTime(&next), gap.ClockAmbiguous, gap.ID); err != nil {
		return err
	}
	if !reolink.CameraClockEstimated(gap.SearchTimezone) && !gap.ClockAmbiguous {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT Id, StartAtUtc, EndAtUtc, MissingSeconds FROM RecoveryGaps
			WHERE PeriodId = ? AND Status = 'Provisional' AND SearchTimezone = ? AND ClockAmbiguous = ? ORDER BY julianday(StartAtUtc), Id`, gap.PeriodID, gap.SearchTimezone, gap.ClockAmbiguous)
	if err != nil {
		return err
	}
	type candidate struct {
		id         string
		start, end time.Time
		missing    float64
	}
	var component []candidate
	var componentEnd time.Time
	found := false
	for rows.Next() {
		var item candidate
		var first, last string
		if err := rows.Scan(&item.id, &first, &last, &item.missing); err != nil {
			rows.Close()
			return err
		}
		item.start, err = time.Parse(time.RFC3339Nano, first)
		if err != nil {
			rows.Close()
			return err
		}
		item.end, err = time.Parse(time.RFC3339Nano, last)
		if err != nil {
			rows.Close()
			return err
		}
		if len(component) > 0 && item.start.After(componentEnd) {
			if found {
				break
			}
			component = nil
		}
		component = append(component, item)
		if len(component) == 1 || item.end.After(componentEnd) {
			componentEnd = item.end
		}
		found = found || item.id == gap.ID
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found || len(component) == 0 {
		return errors.New("provisional recovery interval disappeared")
	}
	anchor := component[0].id
	var total float64
	for _, item := range component {
		total += item.missing
		if item.id == anchor {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO RecoveryGapRecordings (GapId, RecordingId) SELECT ?, RecordingId FROM RecoveryGapRecordings WHERE GapId = ?`, anchor, item.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM RecoveryGapRecordings WHERE GapId = ?`, item.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM RecoveryGaps WHERE Id = ?`, item.id); err != nil {
			return err
		}
	}
	total = min(total, componentEnd.Sub(component[0].start).Seconds())
	_, err = tx.ExecContext(ctx, `UPDATE RecoveryGaps SET StartAtUtc = ?, EndAtUtc = ?, MissingSeconds = ? WHERE Id = ?`, formatTime(&component[0].start), formatTime(&componentEnd), total, anchor)
	return err
}

func (store *Store) UpdateRecoveryGap(ctx context.Context, id, status string, missing float64, now time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		return updateRecoveryGapTx(ctx, tx, id, status, missing, now)
	})
}

func updateRecoveryGapTx(ctx context.Context, tx *sql.Tx, id, status string, missing float64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE RecoveryGaps SET Status = ?, MissingSeconds = ?, LastCheckedAtUtc = ? WHERE Id = ?`, status, missing, formatTime(&now), id); err != nil {
		return err
	}
	if status == "CheckedEvents" {
		var linked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RecoveryGapRecordings WHERE GapId = ?)`, id).Scan(&linked); err != nil {
			return err
		}
		if linked {
			return nil
		}
		var periodID, start, end string
		if err := tx.QueryRowContext(ctx, `SELECT PeriodId, StartAtUtc, EndAtUtc FROM RecoveryGaps WHERE Id = ?`, id).Scan(&periodID, &start, &end); err != nil {
			return err
		}
		var mergedStart, mergedEnd string
		if err := tx.QueryRowContext(ctx, `SELECT MIN(StartAtUtc), MAX(EndAtUtc) FROM RecoveryGaps
				WHERE PeriodId = ? AND Status = 'CheckedEvents'
				AND julianday(StartAtUtc) <= julianday(?) AND julianday(EndAtUtc) >= julianday(?)
				AND NOT EXISTS (SELECT 1 FROM RecoveryGapRecordings WHERE GapId = RecoveryGaps.Id)`, periodID, end, start).Scan(&mergedStart, &mergedEnd); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE RecoveryGaps SET StartAtUtc = ?, EndAtUtc = ? WHERE Id = ?`, mergedStart, mergedEnd, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM RecoveryGaps WHERE PeriodId = ? AND Id != ? AND Status = 'CheckedEvents'
				AND julianday(StartAtUtc) >= julianday(?) AND julianday(EndAtUtc) <= julianday(?)
				AND NOT EXISTS (SELECT 1 FROM RecoveryGapRecordings WHERE GapId = RecoveryGaps.Id)`, periodID, id, mergedStart, mergedEnd); err != nil {
			return err
		}
	}
	cutoff := now.AddDate(0, 0, -30)
	terminal := `SELECT Id FROM RecoveryGaps WHERE Status IN ('Recovered', 'RecoveredEvents', 'CheckedEvents')
			AND julianday(COALESCE(LastCheckedAtUtc, EndAtUtc)) < julianday(?)`
	if _, err := tx.ExecContext(ctx, `DELETE FROM RecoveryGapRecordings WHERE GapId IN (`+terminal+`)`, formatTime(&cutoff)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM RecoveryGaps WHERE Id IN (`+terminal+`)`, formatTime(&cutoff))
	return err
}

func (store *Store) QueueRecoveryRecordings(ctx context.Context, gap domain.RecoveryGap, recordings []domain.Recording, now time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		return queueRecoveryRecordingsTx(ctx, tx, gap, recordings, now)
	})
}

func queueRecoveryRecordingsTx(ctx context.Context, tx *sql.Tx, gap domain.RecoveryGap, recordings []domain.Recording, now time.Time) error {
	run := domain.IngestionRun{SearchClock: gap.SearchTimezone, SearchEndpoint: gap.SearchEndpoint,
		ID: fmt.Sprintf("%s-%d", gap.ID, now.UnixNano()), CameraID: gap.CameraID, Kind: "Recovery", WindowStartUtc: gap.StartAtUtc, WindowEndUtc: gap.EndAtUtc, Status: "Complete", DiscoveredCount: len(recordings), StartedAtUtc: now, CompletedAtUtc: &now}
	if run.SearchEndpoint == "" {
		var host string
		var port, channel int
		if err := tx.QueryRowContext(ctx, `SELECT Host, Port, Channel FROM Cameras WHERE Id = ?`, gap.CameraID).Scan(&host, &port, &channel); err != nil {
			return err
		}
		run.SearchEndpoint = reolink.CameraEndpointIdentity(host, port, channel)
	}
	if err := saveReconciliationTx(ctx, tx, run, recordings, now); err != nil {
		return err
	}
	for _, recording := range recordings {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO RecoveryGapRecordings (GapId, RecordingId)
			SELECT ?, Id FROM Recordings WHERE Id = ? AND (Source != 'CameraStorage' OR
			json_extract(CASE WHEN json_valid(RawMetadataJson) THEN RawMetadataJson ELSE '{}' END, '$._reonvrEndpoint') = ?)`, gap.ID, recording.ID, run.SearchEndpoint); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE Recordings SET State = 'Queued' WHERE Id = ? AND State = 'Complete'
				AND (Source != 'CameraStorage' OR json_extract(CASE WHEN json_valid(RawMetadataJson) THEN RawMetadataJson ELSE '{}' END, '$._reonvrEndpoint') = ?)
				AND NOT EXISTS (SELECT 1 FROM RecordingCoverage WHERE RecordingId = Recordings.Id)
				AND COALESCE(json_extract(CASE WHEN json_valid(RawMetadataJson) THEN RawMetadataJson ELSE '{}' END,
				'$._reonvrSidecarPending'), 0) != 1`, recording.ID, run.SearchEndpoint); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) CommitRecoverySearch(ctx context.Context, gap domain.RecoveryGap, recordings []domain.Recording, status string, missing float64, searchedUntil, now time.Time, provisional bool) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		var timezone, host string
		var port, channel int
		if err := tx.QueryRowContext(ctx, `SELECT Timezone, Host, Port, Channel FROM Cameras WHERE Id = ? AND RetiredAtUtc IS NULL`, gap.CameraID).Scan(&timezone, &host, &port, &channel); err != nil {
			return err
		}
		if timezone != gap.SearchTimezone || reolink.CameraEndpointIdentity(host, port, channel) != gap.SearchEndpoint {
			_, err := tx.ExecContext(ctx, `UPDATE RecoveryGaps SET Status = 'Pending', SearchTimezone = '', SearchCursorUtc = NULL, NextSearchAtUtc = NULL WHERE Id = ?`, gap.ID)
			return err
		}
		if len(recordings) > 0 {
			if err := queueRecoveryRecordingsTx(ctx, tx, gap, recordings, now); err != nil {
				return err
			}
		}
		if provisional {
			return updateProvisionalRecoveryGapTx(ctx, tx, gap, searchedUntil, missing, now)
		}
		return updateRecoveryGapTx(ctx, tx, gap.ID, status, missing, now)
	})
}

func (store *Store) loadRecoveryStats(ctx context.Context, camera *domain.Camera) error {
	camera.RecoveryStatus = "Disabled"
	if !camera.RecoveryEnabled {
		return nil
	}
	camera.RecoveryStatus = "Monitoring"
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(Gaps.MissingSeconds), 0)
		FROM RecoveryGaps Gaps JOIN RecoveryPeriods Periods ON Periods.Id = Gaps.PeriodId
		WHERE Periods.CameraId = ? AND Gaps.Status NOT IN ('Recovered', 'RecoveredEvents', 'CheckedEvents')`, camera.ID).Scan(&camera.RecoveryPendingCount, &camera.RecoveryMissingSeconds); err != nil {
		return err
	}
	if camera.RecoveryPendingCount > 0 {
		camera.RecoveryStatus = "Pending"
	}
	if camera.Capabilities == nil || !camera.Capabilities.StorageAvailable {
		camera.RecoveryStatus = "StorageUnavailable"
	} else if !camera.Capabilities.StoragePlaybackAvailable {
		camera.RecoveryStatus = "SearchUnavailable"
	}
	return nil
}

func (store *Store) RecoveryRecordingFinal(ctx context.Context, recordingID string) (bool, error) {
	var final bool
	err := store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM Recordings WHERE Id = ? AND
		(State IN ('Deleted', 'Expired', 'Failed') OR (State = 'Complete' AND EXISTS (SELECT 1 FROM RecordingCoverage WHERE RecordingId = Recordings.Id))))`, recordingID).Scan(&final)
	return final, err
}
