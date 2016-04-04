// Copyright (c) 2012-2015 The upper.io/db authors. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining
// a copy of this software and associated documentation files (the
// "Software"), to deal in the Software without restriction, including
// without limitation the rights to use, copy, modify, merge, publish,
// distribute, sublicense, and/or sell copies of the Software, and to
// permit persons to whom the Software is furnished to do so, subject to
// the following conditions:
//
// The above copyright notice and this permission notice shall be
// included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
// NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
// LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
// OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
// WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package sqlite

import (
	"errors"
	"fmt"
	"sync/atomic"

	"database/sql"

	_ "github.com/mattn/go-sqlite3" // SQLite3 driver.
	"upper.io/db.v2"
	"upper.io/db.v2/builder"
	"upper.io/db.v2/builder/sqlgen"
	"upper.io/db.v2/internal/sqladapter"
)

type database struct {
	*sqladapter.BaseDatabase
	columns map[string][]columnSchemaT
}

var (
	fileOpenCount       int32
	errTooManyOpenFiles = errors.New(`Too many open database files.`)
)

type columnSchemaT struct {
	Name string `db:"name"`
	PK   int    `db:"pk"`
}

var _ = db.Database(&database{})

const (
	// If we try to open lots of sessions cgo will panic without a warning, this
	// artificial limit was added to prevent that panic.
	maxOpenFiles = 100
)

// CompileAndReplacePlaceholders compiles the given statement into an string
// and replaces each generic placeholder with the placeholder the driver
// expects (if any).
func (d *database) CompileAndReplacePlaceholders(stmt *sqlgen.Statement) (query string) {
	return stmt.Compile(d.Template())
}

// Err translates some known errors into generic errors.
func (d *database) Err(err error) error {
	if err != nil {
		if err == errTooManyOpenFiles {
			return db.ErrTooManyClients
		}
	}
	return err
}

func (d *database) open() error {
	var sess *sql.DB

	openFn := func(sess **sql.DB) (err error) {
		openFiles := atomic.LoadInt32(&fileOpenCount)

		if openFiles < maxOpenFiles {
			*sess, err = sql.Open(`sqlite3`, d.ConnectionURL().String())

			if err == nil {
				atomic.AddInt32(&fileOpenCount, 1)
			}
			return
		}

		return errTooManyOpenFiles

	}

	if err := d.WaitForConnection(func() error { return openFn(&sess) }); err != nil {
		return err
	}

	return d.Bind(sess)
}

// Open attempts to open a connection to the database server.
func (d *database) Open(connURL db.ConnectionURL) error {
	d.BaseDatabase = sqladapter.NewDatabase(d, connURL, template())
	return d.open()
}

func (d *database) Close() error {
	if d.Session() != nil {
		if atomic.AddInt32(&fileOpenCount, -1) < 0 {
			return errors.New(`Close() without Open()?`)
		}
		return d.BaseDatabase.Close()
	}
	return nil
}

// Clone creates a new database connection with the same settings as the
// original.
func (d *database) Clone() (db.Database, error) {
	return d.clone()
}

// NewTable returns a db.Collection.
func (d *database) NewTable(name string) db.Collection {
	return newTable(d, name)
}

// Collections returns a list of non-system tables from the database.
func (d *database) Collections() (collections []string, err error) {
	q := d.Builder().Select("tbl_name").
		From("sqlite_master").
		Where("type = ?", "table")

	iter := q.Iterator()
	defer iter.Close()

	for iter.Next() {
		var tableName string
		if err := iter.Scan(&tableName); err != nil {
			return nil, err
		}
		collections = append(collections, tableName)
	}

	return collections, nil
}

// Transaction starts a transaction block and returns a db.Tx struct that can
// be used to issue transactional queries.
func (d *database) Transaction() (db.Tx, error) {
	var err error
	var sqlTx *sql.Tx
	var clone *database

	if clone, err = d.clone(); err != nil {
		return nil, err
	}

	connFn := func(sqlTx **sql.Tx) (err error) {
		*sqlTx, err = clone.Session().Begin()
		return
	}

	if err := d.WaitForConnection(func() error { return connFn(&sqlTx) }); err != nil {
		return nil, err
	}

	clone.BindTx(sqlTx)

	return &sqladapter.TxDatabase{Database: clone, Tx: clone.Tx()}, nil
}

// PopulateSchema looks up for the table info in the database and populates its
// schema for internal use.
func (d *database) PopulateSchema() (err error) {
	schema := d.NewSchema()

	var connURL ConnectionURL
	if connURL, err = ParseURL(d.ConnectionURL().String()); err != nil {
		return err
	}

	schema.SetName(connURL.Database)

	return nil
}

// TableExists checks whether a table exists and returns an error in case it doesn't.
func (d *database) TableExists(name string) error {
	q := d.Builder().Select("tbl_name").
		From("sqlite_master").
		Where("type = 'table' AND tbl_name = ?", name)

	iter := q.Iterator()
	defer iter.Close()

	if iter.Next() {
		var name string
		if err := iter.Scan(&name); err != nil {
			return err
		}
		return nil
	}
	return db.ErrCollectionDoesNotExist
}

// TablePrimaryKey returns all primary keys from the given table.
func (d *database) TablePrimaryKey(tableName string) ([]string, error) {
	tableSchema := d.Schema().Table(tableName)

	pk := tableSchema.PrimaryKeys()
	if pk != nil {
		return pk, nil
	}

	pk = []string{}

	stmt := sqlgen.RawSQL(fmt.Sprintf(`PRAGMA TABLE_INFO('%s')`, tableName))

	rows, err := d.Builder().Query(stmt)
	if err != nil {
		return nil, err
	}

	if d.columns == nil {
		d.columns = make(map[string][]columnSchemaT)
	}

	columns := []columnSchemaT{}

	if err := builder.NewIterator(rows).All(&columns); err != nil {
		return nil, err
	}

	maxValue := -1

	for _, column := range columns {
		if column.PK > 0 && column.PK > maxValue {
			maxValue = column.PK
		}
	}

	if maxValue > 0 {
		for _, column := range columns {
			if column.PK > 0 {
				pk = append(pk, column.Name)
			}
		}
	}

	tableSchema.SetPrimaryKeys(pk)

	return pk, nil
}

func (d *database) clone() (*database, error) {
	clone := &database{}
	clone.BaseDatabase = d.BaseDatabase.Clone(clone)
	if err := clone.open(); err != nil {
		return nil, err
	}
	return clone, nil
}
