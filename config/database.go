// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package config

import (
	"bytes"
	"database/sql"
	"io/ioutil"
	"regexp"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"

	// Load the MySQL driver
	_ "github.com/go-sql-driver/mysql"
	// Load the Postgres driver
	_ "github.com/lib/pq"
)

// MaxWriteLength defines the maximum length accepted for write to the Configurations or
// ConfigurationFiles table.
//
// It is imposed by MySQL's default max_allowed_packet value of 4Mb.
const MaxWriteLength = 4 * 1024 * 1024

var tcpStripper = regexp.MustCompile(`@tcp\((.*)\)`)

// DatabaseStore is a config store backed by a database.
type DatabaseStore struct {
	commonStore

	originalDsn    string
	driverName     string
	dataSourceName string
	db             *sqlx.DB
}

// NewDatabaseStore creates a new instance of a config store backed by the given database.
func NewDatabaseStore(dsn string) (ds *DatabaseStore, err error) {
	driverName, dataSourceName, err := parseDSN(dsn)
	if err != nil {
		return nil, errors.Wrap(err, "invalid DSN")
	}

	db, err := sqlx.Open(driverName, dataSourceName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to connect to %s database", driverName)
	}

	ds = &DatabaseStore{
		driverName:     driverName,
		originalDsn:    dsn,
		dataSourceName: dataSourceName,
		db:             db,
	}
	if err = initializeConfigurationsTable(ds.db); err != nil {
		return nil, errors.Wrap(err, "failed to initialize")
	}

	if err = ds.Load(); err != nil {
		return nil, errors.Wrap(err, "failed to load")
	}

	return ds, nil
}

// initializeConfigurationsTable ensures the requisite tables in place to form the backing store.
//
// Uses MEDIUMTEXT on MySQL, and TEXT on sane databases.
func initializeConfigurationsTable(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS Configurations (
		    Id VARCHAR(26) PRIMARY KEY,
		    Value TEXT NOT NULL,
		    CreateAt BIGINT NOT NULL,
		    Active BOOLEAN NULL UNIQUE
		)
	`)
	if err != nil {
		return errors.Wrap(err, "failed to create Configurations table")
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS ConfigurationFiles (
		    Name VARCHAR(64) PRIMARY KEY,
		    Data TEXT NOT NULL,
		    CreateAt BIGINT NOT NULL,
		    UpdateAt BIGINT NOT NULL
		)
	`)
	if err != nil {
		return errors.Wrap(err, "failed to create ConfigurationFiles table")
	}

	// Change from TEXT (65535 limit) to MEDIUM TEXT (16777215) on MySQL. This is a
	// backwards-compatible migration for any existing schema.
	if db.DriverName() == "mysql" {
		_, err = db.Exec(`ALTER TABLE Configurations MODIFY Value MEDIUMTEXT`)
		if err != nil {
			return errors.Wrap(err, "failed to alter Configurations table")
		}

		_, err = db.Exec(`ALTER TABLE ConfigurationFiles MODIFY Data MEDIUMTEXT`)
		if err != nil {
			return errors.Wrap(err, "failed to alter ConfigurationFiles table")
		}
	}

	return nil
}

// parseDSN splits up a connection string into a driver name and data source name.
//
// For example:
//	mysql://mmuser:mostest@localhost:5432/mattermost_test
// returns
//	driverName = mysql
//	dataSourceName = mmuser:mostest@localhost:5432/mattermost_test
//
// By contrast, a Postgres DSN is returned unmodified.
func parseDSN(dsn string) (string, string, error) {
	// Treat the DSN as the URL that it is.
	s := strings.SplitN(dsn, "://", 2)
	if len(s) != 2 {
		errors.New("failed to parse DSN as URL")
	}

	scheme := s[0]
	switch scheme {
	case "mysql":
		// Strip off the mysql:// for the dsn with which to connect.
		dsn = s[1]

	case "postgres":
		// No changes required

	default:
		return "", "", errors.Errorf("unsupported scheme %s", scheme)
	}

	return scheme, dsn, nil
}

// Set replaces the current configuration in its entirety and updates the backing store.
func (ds *DatabaseStore) Set(newCfg *model.Config) (*model.Config, error) {
	return ds.commonStore.set(newCfg, true, ds.commonStore.validate, ds.persist)
}

// maxLength identifies the maximum length of a configuration or configuration file
func (ds *DatabaseStore) checkLength(length int) error {
	if ds.db.DriverName() == "mysql" && length > MaxWriteLength {
		return errors.Errorf("value is too long: %d > %d bytes", length, MaxWriteLength)
	}

	return nil
}

// persist writes the configuration to the configured database.
func (ds *DatabaseStore) persist(cfg *model.Config) error {
	b, err := marshalConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "failed to serialize")
	}

	id := model.NewId()
	value := string(b)
	createAt := model.GetMillis()

	err = ds.checkLength(len(value))
	if err != nil {
		return errors.Wrap(err, "marshalled configuration failed length check")
	}

	tx, err := ds.db.Beginx()
	if err != nil {
		return errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		// Rollback after Commit just returns sql.ErrTxDone.
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			mlog.Error("Failed to rollback configuration transaction", mlog.Err(err))
		}
	}()

	params := map[string]interface{}{
		"id":        id,
		"value":     value,
		"create_at": createAt,
		"key":       "ConfigurationId",
	}

	// Skip the persist altogether if we're effectively writing the same configuration.
	var oldValue []byte
	row := ds.db.QueryRow("SELECT Value FROM Configurations WHERE Active")
	if err := row.Scan(&oldValue); err != nil && err != sql.ErrNoRows {
		return errors.Wrap(err, "failed to query active configuration")
	}
	if bytes.Equal(oldValue, b) {
		return nil
	}

	if _, err := tx.Exec("UPDATE Configurations SET Active = NULL WHERE Active"); err != nil {
		return errors.Wrap(err, "failed to deactivate current configuration")
	}

	if _, err := tx.NamedExec("INSERT INTO Configurations (Id, Value, CreateAt, Active) VALUES (:id, :value, :create_at, TRUE)", params); err != nil {
		return errors.Wrap(err, "failed to record new configuration")
	}

	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "failed to commit transaction")
	}

	return nil
}

// Load updates the current configuration from the backing store.
func (ds *DatabaseStore) Load() (err error) {
	var needsSave bool
	var configurationData []byte

	row := ds.db.QueryRow("SELECT Value FROM Configurations WHERE Active")
	if err = row.Scan(&configurationData); err != nil && err != sql.ErrNoRows {
		return errors.Wrap(err, "failed to query active configuration")
	}

	// Initialize from the default config if no active configuration could be found.
	if len(configurationData) == 0 {
		needsSave = true

		defaultCfg := &model.Config{}
		defaultCfg.SetDefaults()

		// Assume the database storing the config is also to be used for the application.
		// This can be overridden using environment variables on first start if necessary,
		// or changed from the system console afterwards.
		*defaultCfg.SqlSettings.DriverName = ds.driverName
		*defaultCfg.SqlSettings.DataSource = ds.dataSourceName

		configurationData, err = marshalConfig(defaultCfg)
		if err != nil {
			return errors.Wrap(err, "failed to serialize default config")
		}
	}

	return ds.commonStore.load(ioutil.NopCloser(bytes.NewReader(configurationData)), needsSave, ds.commonStore.validate, ds.persist)
}

// GetFile fetches the contents of a previously persisted configuration file.
func (ds *DatabaseStore) GetFile(name string) ([]byte, error) {
	query, args, err := sqlx.Named("SELECT Data FROM ConfigurationFiles WHERE Name = :name", map[string]interface{}{
		"name": name,
	})
	if err != nil {
		return nil, err
	}

	var data []byte
	row := ds.db.QueryRowx(query, args...)
	if err = row.Scan(&data); err != nil {
		return nil, errors.Wrapf(err, "failed to scan data from row for %s", name)
	}

	return data, nil
}

// SetFile sets or replaces the contents of a configuration file.
func (ds *DatabaseStore) SetFile(name string, data []byte) error {
	err := ds.checkLength(len(data))
	if err != nil {
		return errors.Wrap(err, "file data failed length check")
	}

	params := map[string]interface{}{
		"name":      name,
		"data":      data,
		"create_at": model.GetMillis(),
		"update_at": model.GetMillis(),
	}

	result, err := ds.db.NamedExec("UPDATE ConfigurationFiles SET Data = :data, UpdateAt = :update_at WHERE Name = :name", params)
	if err != nil {
		return errors.Wrapf(err, "failed to update row for %s", name)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return errors.Wrapf(err, "failed to count rows affected for %s", name)
	} else if count > 0 {
		return nil
	}

	_, err = ds.db.NamedExec("INSERT INTO ConfigurationFiles (Name, Data, CreateAt, UpdateAt) VALUES (:name, :data, :create_at, :update_at)", params)
	if err != nil {
		return errors.Wrapf(err, "failed to insert row for %s", name)
	}

	return nil
}

// HasFile returns true if the given file was previously persisted.
func (ds *DatabaseStore) HasFile(name string) (bool, error) {
	query, args, err := sqlx.Named("SELECT COUNT(*) FROM ConfigurationFiles WHERE Name = :name", map[string]interface{}{
		"name": name,
	})
	if err != nil {
		return false, err
	}

	var count int
	row := ds.db.QueryRowx(query, args...)
	if err = row.Scan(&count); err != nil {
		return false, errors.Wrapf(err, "failed to scan count of rows for %s", name)
	}

	return count != 0, nil
}

// RemoveFile remoevs a previously persisted configuration file.
func (ds *DatabaseStore) RemoveFile(name string) error {
	_, err := ds.db.NamedExec("DELETE FROM ConfigurationFiles WHERE Name = :name", map[string]interface{}{
		"name": name,
	})
	if err != nil {
		return errors.Wrapf(err, "failed to remove row for %s", name)
	}

	return nil
}

// String returns the path to the database backing the config, masking the password.
func (ds *DatabaseStore) String() string {
	return stripPassword(ds.originalDsn, ds.driverName)
}

// Close cleans up resources associated with the store.
func (ds *DatabaseStore) Close() error {
	ds.configLock.Lock()
	defer ds.configLock.Unlock()

	return ds.db.Close()
}
