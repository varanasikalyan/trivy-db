package db

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/xerrors"

	"github.com/aquasecurity/trivy-db/pkg/log"
	"github.com/aquasecurity/trivy-db/pkg/types"
)

type CustomPut func(dbc Operation, tx *bolt.Tx, adv interface{}) error

const SchemaVersion = 2

var (
	db    *bolt.DB
	dbDir string
)

type Operation interface {
	BatchUpdate(fn func(*bolt.Tx) error) (err error)

	GetVulnerabilityDetail(cveID string) (detail map[types.SourceID]types.VulnerabilityDetail, err error)
	PutVulnerabilityDetail(tx *bolt.Tx, vulnerabilityID string, source types.SourceID,
		vulnerability types.VulnerabilityDetail) (err error)
	DeleteVulnerabilityDetailBucket() (err error)

	ForEachAdvisory(sources []string, pkgName string) (value map[string]Value, err error)
	GetAdvisories(source string, pkgName string) (advisories []types.Advisory, err error)

	PutVulnerabilityID(tx *bolt.Tx, vulnerabilityID string) (err error)
	ForEachVulnerabilityID(fn func(tx *bolt.Tx, cveID string) error) (err error)

	PutVulnerability(tx *bolt.Tx, vulnerabilityID string, vulnerability types.Vulnerability) (err error)
	GetVulnerability(vulnerabilityID string) (vulnerability types.Vulnerability, err error)

	SaveAdvisoryDetails(tx *bolt.Tx, cveID string) (err error)
	PutAdvisoryDetail(tx *bolt.Tx, vulnerabilityID, pkgName string, nestedBktNames []string, advisory interface{}) (err error)
	DeleteAdvisoryDetailBucket() error

	PutDataSource(tx *bolt.Tx, bktName string, source types.DataSource) (err error)

	// For Red Hat
	PutRedHatRepositories(tx *bolt.Tx, repository string, cpeIndices []int) (err error)
	PutRedHatNVRs(tx *bolt.Tx, nvr string, cpeIndices []int) (err error)
	PutRedHatCPEs(tx *bolt.Tx, cpeIndex int, cpe string) (err error)
	RedHatRepoToCPEs(repository string) (cpeIndices []int, err error)
	RedHatNVRToCPEs(nvr string) (cpeIndices []int, err error)
}

type Config struct {
}

type BoltOptions struct {
	// Timeout is the amount of time to wait to obtain a file lock.
	// When set to zero it will wait indefinitely. This option is only
	// available on Darwin and Linux.
	Timeout time.Duration

	// Sets the DB.NoGrowSync flag before memory mapping the file.
	NoGrowSync bool

	// Do not sync freelist to disk. This improves the database write performance
	// under normal operation, but requires a full database re-sync during recovery.
	NoFreelistSync bool

	// PreLoadFreelist sets whether to load the free pages when opening
	// the db file. Note when opening db in write mode, bbolt will always
	// load the free pages.
	PreLoadFreelist bool

	// Open database in read-only mode. Uses flock(..., LOCK_SH |LOCK_NB) to
	// grab a shared lock (UNIX).
	ReadOnly bool

	// Sets the DB.MmapFlags flag before memory mapping the file.
	MmapFlags int

	// InitialMmapSize is the initial mmap size of the database
	// in bytes. Read transactions won't block write transaction
	// if the InitialMmapSize is large enough to hold database mmap
	// size. (See DB.Begin for more information)
	//
	// If <=0, the initial map size is 0.
	// If initialMmapSize is smaller than the previous database size,
	// it takes no effect.
	InitialMmapSize int

	// PageSize overrides the default OS page size.
	PageSize int

	// NoSync sets the initial value of DB.NoSync. Normally this can just be
	// set directly on the DB itself when returned from Open(), but this option
	// is useful in APIs which expose Options but not the underlying DB.
	NoSync bool

	// OpenFile is used to open files. It defaults to os.OpenFile. This option
	// is useful for writing hermetic tests.
	OpenFile func(string, int, os.FileMode) (*os.File, error)

	// Mlock locks database file in memory when set to true.
	// It prevents potential page faults, however
	// used memory can't be reclaimed. (UNIX only)
	Mlock bool

	Enable bool
}

func GetBoltOptions(options BoltOptions) *bolt.Options {
	if options.Enable {
		return &bolt.Options{
			ReadOnly: options.ReadOnly,
		}
	}
	return nil
}
func Init(cacheDir string, options BoltOptions) (err error) {
	dbPath := Path(cacheDir)
	dbDir = filepath.Dir(dbPath)
	if err = os.MkdirAll(dbDir, 0700); err != nil {
		return xerrors.Errorf("failed to mkdir: %w", err)
	}

	// bbolt sometimes occurs the fatal error of "unexpected fault address".
	// In that case, the local DB should be broken and needs to be removed.
	debug.SetPanicOnFault(true)
	defer func() {
		if r := recover(); r != nil {
			if err = os.Remove(dbPath); err != nil {
				return
			}
			db, err = bolt.Open(dbPath, 0600, nil)
		}
		debug.SetPanicOnFault(false)
	}()

	boltOptions := GetBoltOptions(options)
	db, err = bolt.Open(dbPath, 0600, boltOptions)
	if err != nil {
		return xerrors.Errorf("failed to open db: %w", err)
	}
	return nil
}

func Dir(cacheDir string) string {
	return filepath.Join(cacheDir, "db")
}

func Path(cacheDir string) string {
	dbPath := filepath.Join(Dir(cacheDir), "trivy.db")
	return dbPath
}

func Close() error {
	// Skip closing the database if the connection is not established.
	if db == nil {
		return nil
	}
	if err := db.Close(); err != nil {
		return xerrors.Errorf("failed to close DB: %w", err)
	}
	return nil
}

func (dbc Config) Connection() *bolt.DB {
	return db
}

func (dbc Config) BatchUpdate(fn func(tx *bolt.Tx) error) error {
	err := db.Batch(fn)
	if err != nil {
		return xerrors.Errorf("error in batch update: %w", err)
	}
	return nil
}

func (dbc Config) put(tx *bolt.Tx, bktNames []string, key string, value interface{}) error {
	if len(bktNames) == 0 {
		return xerrors.Errorf("empty bucket name")
	}

	bkt, err := tx.CreateBucketIfNotExists([]byte(bktNames[0]))
	if err != nil {
		return xerrors.Errorf("failed to create '%s' bucket: %w", bktNames[0], err)
	}

	for _, bktName := range bktNames[1:] {
		bkt, err = bkt.CreateBucketIfNotExists([]byte(bktName))
		if err != nil {
			return xerrors.Errorf("failed to create a bucket: %w", err)
		}
	}
	v, err := json.Marshal(value)
	if err != nil {
		return xerrors.Errorf("failed to unmarshal JSON: %w", err)
	}

	return bkt.Put([]byte(key), v)
}

func (dbc Config) get(bktNames []string, key string) (value []byte, err error) {
	err = db.View(func(tx *bolt.Tx) error {
		if len(bktNames) == 0 {
			return xerrors.Errorf("empty bucket name")
		}

		bkt := tx.Bucket([]byte(bktNames[0]))
		if bkt == nil {
			return nil
		}
		for _, bktName := range bktNames[1:] {
			bkt = bkt.Bucket([]byte(bktName))
			if bkt == nil {
				return nil
			}
		}
		dbValue := bkt.Get([]byte(key))

		// Copy the byte slice so it can be used outside of the current transaction
		value = make([]byte, len(dbValue))
		copy(value, dbValue)

		return nil
	})
	if err != nil {
		return nil, xerrors.Errorf("failed to get data from db: %w", err)
	}
	return value, nil
}

type Value struct {
	Source  types.DataSource
	Content []byte
}

func (dbc Config) forEach(bktNames []string) (map[string]Value, error) {
	if len(bktNames) < 2 {
		return nil, xerrors.Errorf("bucket must be nested: %v", bktNames)
	}
	rootBucket, nestedBuckets := bktNames[0], bktNames[1:]

	values := map[string]Value{}
	err := db.View(func(tx *bolt.Tx) error {
		var rootBuckets []string

		if strings.Contains(rootBucket, "::") {
			// e.g. "pip::", "rubygems::"
			prefix := []byte(rootBucket)
			c := tx.Cursor()
			for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				rootBuckets = append(rootBuckets, string(k))
			}
		} else {
			// e.g. "GitHub Security Advisory Composer"
			rootBuckets = append(rootBuckets, rootBucket)
		}

		for _, r := range rootBuckets {
			root := tx.Bucket([]byte(r))
			if root == nil {
				continue
			}

			source, err := dbc.getDataSource(tx, r)
			if err != nil {
				log.Logger.Debugf("Data source error: %s", err)
			}

			bkt := root
			for _, nestedBkt := range nestedBuckets {
				bkt = bkt.Bucket([]byte(nestedBkt))
				if bkt == nil {
					break
				}
			}
			if bkt == nil {
				continue
			}

			err = bkt.ForEach(func(k, v []byte) error {
				// Copy the byte slice so it can be used outside of the current transaction
				copiedContent := make([]byte, len(v))
				copy(copiedContent, v)

				values[string(k)] = Value{
					Source:  source,
					Content: copiedContent,
				}
				return nil
			})
			if err != nil {
				return xerrors.Errorf("db foreach error: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, xerrors.Errorf("failed to get all key/value in the specified bucket: %w", err)
	}
	return values, nil
}

func (dbc Config) deleteBucket(bucketName string) error {
	return db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(bucketName)); err != nil {
			return xerrors.Errorf("failed to delete bucket: %w", err)
		}
		return nil
	})
}
