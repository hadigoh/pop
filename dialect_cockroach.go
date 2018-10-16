package pop

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	// Load CockroachdbQL/postgres Go driver
	// also loads github.com/lib/pq
	_ "github.com/cockroachdb/cockroach-go/crdb"
	"github.com/gobuffalo/fizz"
	"github.com/gobuffalo/fizz/translators"
	"github.com/gobuffalo/pop/columns"
	"github.com/gobuffalo/pop/logging"
	"github.com/jmoiron/sqlx"
	"github.com/markbates/going/defaults"
	"github.com/pkg/errors"
)

func init() {
	AvailableDialects = append(AvailableDialects, "cockroach")
}

var _ dialect = &cockroach{}

type cockroach struct {
	translateCache    map[string]string
	mu                sync.Mutex
	ConnectionDetails *ConnectionDetails
}

func (p *cockroach) Name() string {
	return "cockroach"
}

func (p *cockroach) Details() *ConnectionDetails {
	return p.ConnectionDetails
}

func (p *cockroach) Create(s store, model *Model, cols columns.Columns) error {
	keyType := model.PrimaryKeyType()
	switch keyType {
	case "int", "int64":
		cols.Remove("id")
		id := struct {
			ID int `db:"id"`
		}{}
		w := cols.Writeable()
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) returning id", model.TableName(), w.String(), w.SymbolizedString())
		log(logging.SQL, query)
		stmt, err := s.PrepareNamed(query)
		if err != nil {
			return errors.WithStack(err)
		}
		err = stmt.Get(&id, model.Value)
		if err != nil {
			if err := stmt.Close(); err != nil {
				return errors.WithMessage(err, "failed to close statement")
			}
			return errors.WithStack(err)
		}
		model.setID(id.ID)
		return errors.WithMessage(stmt.Close(), "failed to close statement")
	}
	return genericCreate(s, model, cols)
}

func (p *cockroach) Update(s store, model *Model, cols columns.Columns) error {
	return genericUpdate(s, model, cols)
}

func (p *cockroach) Destroy(s store, model *Model) error {
	stmt := p.TranslateSQL(fmt.Sprintf("DELETE FROM %s WHERE %s", model.TableName(), model.whereID()))
	err := genericExec(s, stmt, model.ID())
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (p *cockroach) SelectOne(s store, model *Model, query Query) error {
	return genericSelectOne(s, model, query)
}

func (p *cockroach) SelectMany(s store, models *Model, query Query) error {
	return genericSelectMany(s, models, query)
}

func (p *cockroach) CreateDB() error {
	// createdb -h db -p 5432 -U cockroach enterprise_development
	deets := p.ConnectionDetails
	db, err := sql.Open(deets.Dialect, p.urlWithoutDb())
	if err != nil {
		return errors.Wrapf(err, "error creating Cockroach database %s", deets.Database)
	}
	defer db.Close()
	query := fmt.Sprintf("CREATE DATABASE \"%s\"", deets.Database)
	log(logging.SQL, query)

	_, err = db.Exec(query)
	if err != nil {
		return errors.Wrapf(err, "error creating Cockroach database %s", deets.Database)
	}

	log(logging.Info, "created database %s", deets.Database)
	return nil
}

func (p *cockroach) DropDB() error {
	deets := p.ConnectionDetails
	db, err := sql.Open(deets.Dialect, p.urlWithoutDb())
	if err != nil {
		return errors.Wrapf(err, "error dropping Cockroach database %s", deets.Database)
	}
	defer db.Close()
	query := fmt.Sprintf("DROP DATABASE \"%s\" CASCADE;", deets.Database)
	log(logging.SQL, query)

	_, err = db.Exec(query)
	if err != nil {
		return errors.Wrapf(err, "error dropping Cockroach database %s", deets.Database)
	}

	log(logging.Info, "dropped database %s", deets.Database)
	return nil
}

func (p *cockroach) URL() string {
	c := p.ConnectionDetails
	if c.URL != "" {
		return c.URL
	}
	s := "postgres://%s:%s@%s:%s/%s?%s"
	return fmt.Sprintf(s, c.User, c.Password, c.Host, c.Port, c.Database, p.optionString())
}

func (p *cockroach) urlWithoutDb() string {
	c := p.ConnectionDetails
	s := "postgres://%s:%s@%s:%s/?%s"
	return fmt.Sprintf(s, c.User, c.Password, c.Host, c.Port, p.optionString())
}

func (p *cockroach) optionString() string {
	c := p.ConnectionDetails

	if c.RawOptions != "" {
		return c.RawOptions
	}

	s := "application_name=cockroach"
	if c.Options != nil {
		for k := range c.Options {
			s = fmt.Sprintf("%s&%s=%s", s, k, c.Options[k])
		}
	}
	return s
}

func (p *cockroach) MigrationURL() string {
	return p.URL()
}

func (p *cockroach) TranslateSQL(sql string) string {
	defer p.mu.Unlock()
	p.mu.Lock()

	if csql, ok := p.translateCache[sql]; ok {
		return csql
	}
	csql := sqlx.Rebind(sqlx.DOLLAR, sql)

	p.translateCache[sql] = csql
	return csql
}

func (p *cockroach) FizzTranslator() fizz.Translator {
	return translators.NewCockroach(p.URL(), p.Details().Database)
}

func (p *cockroach) Lock(fn func() error) error {
	return fn()
}

func (p *cockroach) DumpSchema(w io.Writer) error {
	cmd := exec.Command("cockroach", "dump", p.Details().Database, "--dump-mode=schema")

	c := p.ConnectionDetails
	if defaults.String(c.Options["sslmode"], "disable") == "disable" {
		cmd.Args = append(cmd.Args, "--insecure")
	}
	log(logging.SQL, strings.Join(cmd.Args, " "))
	cmd.Stdout = w
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return err
	}

	log(logging.Info, "dumped schema for %s", p.Details().Database)
	return nil
}

func (p *cockroach) LoadSchema(r io.Reader) error {
	return genericLoadSchema(p.ConnectionDetails, p.MigrationURL(), r)
}

func (p *cockroach) TruncateAll(tx *Connection) error {
	type table struct {
		TableName string `db:"table_name"`
	}

	var tables []table
	if err := tx.RawQuery("select table_name from information_schema.tables where table_schema = ?;", tx.Dialect.Details().Database).All(&tables); err != nil {
		return err
	}

	if len(tables) == 0 {
		return nil
	}

	tableNames := make([]string, len(tables))
	for i, t := range tables {
		tableNames[i] = t.TableName
	}

	return tx.RawQuery(fmt.Sprintf("truncate %s cascade;", strings.Join(tableNames, ", "))).Exec()
}

func newCockroach(deets *ConnectionDetails) dialect {
	deets.Dialect = "postgres"
	cd := &cockroach{
		ConnectionDetails: deets,
		translateCache:    map[string]string{},
		mu:                sync.Mutex{},
	}
	return cd
}
