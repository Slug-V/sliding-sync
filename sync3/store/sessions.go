package store

import (
	"database/sql"
	"encoding/json"
	"os"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/matrix-org/sync-v3/sqlutil"
	"github.com/matrix-org/sync-v3/sync3"
	"github.com/matrix-org/sync-v3/sync3/streams"
	"github.com/rs/zerolog"
)

var log = zerolog.New(os.Stdout).With().Timestamp().Logger().Output(zerolog.ConsoleWriter{
	Out:        os.Stderr,
	TimeFormat: "15:04:05",
})

type Sessions struct {
	db *sqlx.DB
}

func NewSessions(postgresURI string) *Sessions {
	db, err := sqlx.Open("postgres", postgresURI)
	if err != nil {
		log.Panic().Err(err).Str("uri", postgresURI).Msg("failed to open SQL DB")
	}
	// make sure tables are made
	db.MustExec(`
	CREATE SEQUENCE IF NOT EXISTS syncv3_session_id_seq;
	CREATE TABLE IF NOT EXISTS syncv3_sessions (
		session_id BIGINT PRIMARY KEY DEFAULT nextval('syncv3_session_id_seq'),
		device_id TEXT NOT NULL,
		last_confirmed_token TEXT NOT NULL,
		last_unconfirmed_token TEXT NOT NULL,
		CONSTRAINT syncv3_sessions_unique UNIQUE (device_id, session_id)
	);
	CREATE TABLE IF NOT EXISTS syncv3_sessions_v2devices (
		user_id TEXT NOT NULL,
		device_id TEXT PRIMARY KEY,
		since TEXT NOT NULL
	);
	CREATE SEQUENCE IF NOT EXISTS syncv3_filter_id_seq;
	CREATE TABLE IF NOT EXISTS syncv3_filters (
		filter_id BIGINT PRIMARY KEY DEFAULT nextval('syncv3_filter_id_seq'),
		session_id BIGINT NOT NULL,
		req_json TEXT NOT NULL
	);
	`)
	return &Sessions{
		db: db,
	}
}

func (s *Sessions) NewSession(deviceID string) (*sync3.Session, error) {
	var session *sync3.Session
	err := sqlutil.WithTransaction(s.db, func(txn *sqlx.Tx) error {
		// make a new session
		var id int64
		err := txn.QueryRow(`
			INSERT INTO syncv3_sessions(device_id, last_confirmed_token, last_unconfirmed_token)
			VALUES($1, '', '') RETURNING session_id`,
			deviceID,
		).Scan(&id)
		if err != nil {
			return err
		}
		// make sure there is a device entry for this device ID. If one already exists, don't clobber
		// the since value else we'll forget our position!
		result, err := txn.Exec(`
			INSERT INTO syncv3_sessions_v2devices(device_id, since, user_id) VALUES($1,$2,$3)
			ON CONFLICT (device_id) DO NOTHING`,
			deviceID, "", "",
		)
		if err != nil {
			return err
		}

		// if we inserted a row that means it's a brand new device ergo there is no since token
		if ra, err := result.RowsAffected(); err == nil && ra == 1 {
			// we inserted a new row, no need to query the since value
			session = &sync3.Session{
				ID:       id,
				DeviceID: deviceID,
			}
			return nil
		}

		// Return the since value as we may start a new poller with this session.
		var since string
		var userID string
		err = txn.QueryRow("SELECT since, user_id FROM syncv3_sessions_v2devices WHERE device_id = $1", deviceID).Scan(&since, &userID)
		if err != nil {
			return err
		}
		session = &sync3.Session{
			ID:       id,
			DeviceID: deviceID,
			V2Since:  since,
			UserID:   userID,
		}
		return nil
	})
	return session, err
}

func (s *Sessions) Session(sessionID int64, deviceID string) (*sync3.Session, error) {
	var result sync3.Session
	// Important not just to use sessionID as that can be set by anyone as a query param
	// Only the device ID is secure (it's a hash of the bearer token)
	err := s.db.Get(&result,
		`SELECT last_confirmed_token, last_unconfirmed_token, since, user_id FROM syncv3_sessions
		LEFT JOIN syncv3_sessions_v2devices
		ON syncv3_sessions.device_id = syncv3_sessions_v2devices.device_id
		WHERE session_id=$1 AND syncv3_sessions.device_id=$2`,
		sessionID, deviceID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result.ID = sessionID
	result.DeviceID = deviceID
	return &result, nil
}

func (s *Sessions) ConfirmedSessionTokens(deviceID string) (tokens []string, err error) {
	err = s.db.Select(&tokens, `SELECT last_confirmed_token FROM syncv3_sessions WHERE device_id=$1`, deviceID)
	return
}

// UpdateLastTokens updates the latest tokens for this session
func (s *Sessions) UpdateLastTokens(sessionID int64, confirmed, unconfirmed string) error {
	_, err := s.db.Exec(
		`UPDATE syncv3_sessions SET last_confirmed_token = $1, last_unconfirmed_token = $2 WHERE session_id = $3`,
		confirmed, unconfirmed, sessionID,
	)
	return err
}

func (s *Sessions) UpdateDeviceSince(deviceID, since string) error {
	_, err := s.db.Exec(`UPDATE syncv3_sessions_v2devices SET since = $1 WHERE device_id = $2`, since, deviceID)
	return err
}

func (s *Sessions) UpdateUserIDForDevice(deviceID, userID string) error {
	_, err := s.db.Exec(`UPDATE syncv3_sessions_v2devices SET user_id = $1 WHERE device_id = $2`, userID, deviceID)
	return err
}

// Insert a new filter for this session. The returned filter ID should be inserted into the since token
// so the request filter can be extracted again.
func (s *Sessions) InsertFilter(sessionID int64, req *streams.Request) (filterID int64, err error) {
	j, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	err = s.db.QueryRow(`INSERT INTO syncv3_filters(session_id, req_json) VALUES($1,$2) RETURNING filter_id`, sessionID, string(j)).Scan(&filterID)
	return
}

// Filter returns the filter for the session ID and filter ID given. If a filter ID is given which is unknown, an
// error is returned as filters should always be known to the server.
func (s *Sessions) Filter(sessionID int64, filterID int64) (*streams.Request, error) {
	// we need the session ID to make sure users can't use other user's filters
	var j string
	err := s.db.QueryRow(`SELECT req_json FROM syncv3_filters WHERE session_id=$1 AND filter_id=$2`, sessionID, filterID).Scan(&j)
	if err != nil {
		return nil, err // ErrNoRows is expected and is an error
	}
	var req streams.Request
	if err := json.Unmarshal([]byte(j), &req); err != nil {
		return nil, err
	}
	return &req, nil
}