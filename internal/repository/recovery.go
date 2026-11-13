package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reonavr/internal/domain"
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

func (store *Store) SaveRecordingCoverage(ctx context.Context, recordingID string, ranges []domain.TimeRange) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
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
	})
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
		WHERE Recordings.CameraId = ? AND Recordings.Stream = ? AND Recordings.State IN ('Complete', 'Deleted')`
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
		AND (Events.EndedAtUtc IS NULL OR julianday(Events.EndedAtUtc) >= julianday(?) - Periods.PostEventSeconds / 86400.0)`,
		gap.PeriodID, gap.CameraID, formatTime(&gap.EndAtUtc), formatTime(&gap.StartAtUtc))
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
		var eventType domain.EventType
		var pre, post int
		if err := rows.Scan(&start, &end, &eventType, &pre, &post); err != nil {
			return nil, err
		}
		if !selected[eventType] {
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
	return store.listRecoveryGaps(ctx, cameraID, limit, false)
}

func (store *Store) ListRecoveryHistory(ctx context.Context, cameraID string, limit int) ([]domain.RecoveryGap, error) {
	return store.listRecoveryGaps(ctx, cameraID, limit, true)
}

func (store *Store) listRecoveryGaps(ctx context.Context, cameraID string, limit int, history bool) ([]domain.RecoveryGap, error) {
	query := `SELECT Gaps.Id, Gaps.PeriodId, Periods.Stream, Periods.Mode, Periods.EventTypesJson,
		Gaps.StartAtUtc, Gaps.EndAtUtc, Gaps.Status, Gaps.MissingSeconds, Gaps.LastCheckedAtUtc
		FROM RecoveryGaps Gaps JOIN RecoveryPeriods Periods ON Periods.Id = Gaps.PeriodId
		WHERE Periods.CameraId = ?`
	if history {
		query += ` ORDER BY CASE WHEN Gaps.Status IN ('Recovered', 'RecoveredEvents', 'CheckedEvents') THEN 1 ELSE 0 END,
			Gaps.StartAtUtc DESC LIMIT ?`
	} else {
		query += ` AND Gaps.Status NOT IN ('Recovered', 'RecoveredEvents', 'CheckedEvents') ORDER BY Gaps.LastCheckedAtUtc, Gaps.StartAtUtc LIMIT ?`
	}
	rows, err := store.db.QueryContext(ctx, query, cameraID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	gaps := make([]domain.RecoveryGap, 0)
	for rows.Next() {
		gap := domain.RecoveryGap{CameraID: cameraID}
		var events, start, end string
		var checked sql.NullString
		if err := rows.Scan(&gap.ID, &gap.PeriodID, &gap.Stream, &gap.Mode, &events, &start, &end, &gap.Status, &gap.MissingSeconds, &checked); err != nil {
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
		gaps = append(gaps, gap)
	}
	return gaps, rows.Err()
}

func (store *Store) UpdateRecoveryGap(ctx context.Context, id, status string, missing float64, now time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
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
	})
}

func (store *Store) QueueRecoveryRecordings(ctx context.Context, gap domain.RecoveryGap, recordings []domain.Recording, now time.Time) error {
	return store.withTransaction(ctx, func(tx *sql.Tx) error {
		run := domain.IngestionRun{ID: fmt.Sprintf("%s-%d", gap.ID, now.UnixNano()), CameraID: gap.CameraID, Kind: "Recovery", WindowStartUtc: gap.StartAtUtc, WindowEndUtc: gap.EndAtUtc, Status: "Complete", DiscoveredCount: len(recordings), StartedAtUtc: now, CompletedAtUtc: &now}
		if err := saveReconciliationTx(ctx, tx, run, recordings, now); err != nil {
			return err
		}
		for _, recording := range recordings {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO RecoveryGapRecordings (GapId, RecordingId) VALUES (?, ?)`, gap.ID, recording.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE Recordings SET State = 'Queued' WHERE Id = ? AND State = 'Complete'
				AND NOT EXISTS (SELECT 1 FROM RecordingCoverage WHERE RecordingId = Recordings.Id)`, recording.ID); err != nil {
				return err
			}
		}
		return nil
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
		(State IN ('Deleted', 'Expired') OR (State = 'Complete' AND EXISTS (SELECT 1 FROM RecordingCoverage WHERE RecordingId = Recordings.Id))))`, recordingID).Scan(&final)
	return final, err
}
