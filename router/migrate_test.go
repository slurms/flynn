package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/flynn/flynn/pkg/postgres"
	"github.com/flynn/flynn/pkg/testutils/postgres"
	"github.com/flynn/flynn/pkg/tlscert"
	"github.com/flynn/flynn/router/types"
	"github.com/jackc/pgx"

	. "github.com/flynn/go-check"
)

func setupTestDB(c *C, dbname string) *postgres.DB {
	if err := pgtestutils.SetupPostgres(dbname); err != nil {
		c.Fatal(err)
	}
	pgxpool, err := pgx.NewConnPool(pgx.ConnPoolConfig{
		ConnConfig: pgx.ConnConfig{
			Host:     os.Getenv("PGHOST"),
			Database: dbname,
		},
	})
	if err != nil {
		c.Fatal(err)
	}
	return postgres.New(pgxpool, nil)
}

type MigrateSuite struct{}

var _ = Suite(&MigrateSuite{})

type testMigrator struct {
	c  *C
	db *postgres.DB
	id int
}

func (t *testMigrator) migrateTo(id int) {
	t.c.Assert((*migrations)[t.id:id].Migrate(t.db), IsNil)
	t.id = id
}

func (MigrateSuite) TestMigrateTLSObject(c *C) {
	db := setupTestDB(c, "routertest_tls_object_migration")
	m := &testMigrator{c: c, db: db}

	// start from ID 4
	m.migrateTo(4)

	nRoutes := 5
	routes := make([]*router.Route, nRoutes)
	certs := make([]*tlscert.Cert, nRoutes)
	for i := 0; i < len(routes)-2; i++ {
		cert := tlsConfigForDomain(fmt.Sprintf("migrationtest%d.example.org", i))
		r := &router.Route{
			ParentRef:     fmt.Sprintf("some/parent/ref/%d", i),
			Service:       fmt.Sprintf("migrationtest%d.example.org", i),
			Domain:        fmt.Sprintf("migrationtest%d.example.org", i),
			LegacyTLSCert: cert.CACert,
			LegacyTLSKey:  cert.PrivateKey,
		}
		err := db.QueryRow(`
			INSERT INTO http_routes (parent_ref, service, domain, tls_cert, tls_key)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			r.ParentRef,
			r.Service,
			r.Domain,
			r.LegacyTLSCert,
			r.LegacyTLSKey).Scan(&r.ID)
		c.Assert(err, IsNil)
		routes[i] = r
		certs[i] = cert
	}

	{
		// Add route with leading and trailing whitespace on cert and key
		i := len(routes) - 2
		cert := certs[i-1] // use the same cert as the previous route
		r := &router.Route{
			ParentRef:     fmt.Sprintf("some/parent/ref/%d", i),
			Service:       fmt.Sprintf("migrationtest%d.example.org", i),
			Domain:        fmt.Sprintf("migrationtest%d.example.org", i),
			LegacyTLSCert: "  \n\n  \n " + cert.CACert + "   \n   \n   ",
			LegacyTLSKey:  "    \n   " + cert.PrivateKey + "   \n   \n  ",
		}
		err := db.QueryRow(`
			INSERT INTO http_routes (parent_ref, service, domain, tls_cert, tls_key)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			r.ParentRef,
			r.Service,
			r.Domain,
			r.LegacyTLSCert,
			r.LegacyTLSKey).Scan(&r.ID)
		c.Assert(err, IsNil)
		routes[i] = r
		certs[i] = cert
	}

	{
		// Add route without cert
		i := len(routes) - 1
		r := &router.Route{
			ParentRef: fmt.Sprintf("some/parent/ref/%d", i),
			Service:   fmt.Sprintf("migrationtest%d.example.org", i),
			Domain:    fmt.Sprintf("migrationtest%d.example.org", i),
		}
		err := db.QueryRow(`
			INSERT INTO http_routes (parent_ref, service, domain)
			VALUES ($1, $2, $3) RETURNING id`,
			r.ParentRef,
			r.Service,
			r.Domain).Scan(&r.ID)
		c.Assert(err, IsNil)
		routes[i] = r
	}

	for i, cert := range certs {
		if i == 0 || i >= len(certs)-2 {
			continue
		}
		c.Assert(cert.CACert, Not(Equals), certs[i-1].CACert)
	}

	// run TLS object migration
	m.migrateTo(5)

	for i, r := range routes {
		cert := certs[i]
		fetchedRoute := &router.Route{}
		var fetchedCert *string
		var fetchedCertKey *string
		var fetchedCertSHA256 *string
		err := db.QueryRow(`
			SELECT r.parent_ref, r.service, r.domain, c.cert, c.key, encode(c.cert_sha256, 'hex') FROM http_routes AS r
			LEFT OUTER JOIN route_certificates AS rc ON rc.http_route_id = r.id
			LEFT OUTER JOIN certificates AS c ON rc.certificate_id = c.id
			WHERE r.id = $1
		`, r.ID).Scan(&fetchedRoute.ParentRef, &fetchedRoute.Service, &fetchedRoute.Domain, &fetchedCert, &fetchedCertKey, &fetchedCertSHA256)
		c.Assert(err, IsNil)

		c.Assert(fetchedRoute.ParentRef, Equals, r.ParentRef)
		c.Assert(fetchedRoute.Service, Equals, r.Service)
		c.Assert(fetchedRoute.Domain, Equals, r.Domain)

		if cert == nil {
			// the last route doesn't have a cert
			c.Assert(fetchedCert, IsNil)
			c.Assert(fetchedCertKey, IsNil)
			c.Assert(fetchedCertSHA256, IsNil)
		} else {
			sum := sha256.Sum256([]byte(strings.TrimSpace(cert.CACert)))
			certSHA256 := hex.EncodeToString(sum[:])
			c.Assert(fetchedCert, Not(IsNil))
			c.Assert(fetchedCertKey, Not(IsNil))
			c.Assert(fetchedCertSHA256, Not(IsNil))
			c.Assert(strings.TrimSpace(*fetchedCert), Equals, strings.TrimSpace(cert.CACert))
			c.Assert(strings.TrimSpace(*fetchedCertKey), Equals, strings.TrimSpace(cert.PrivateKey))
			c.Assert(*fetchedCertSHA256, Equals, certSHA256)
		}
	}

	var count int64
	err := db.QueryRow(`SELECT COUNT(*) FROM certificates`).Scan(&count)
	c.Assert(err, IsNil)
	// the last two certs are the same and there's one nil after them
	c.Assert(count, Equals, int64(len(certs)-2))

	err = db.QueryRow(`SELECT COUNT(*) FROM http_routes`).Scan(&count)
	c.Assert(err, IsNil)
	c.Assert(count, Equals, int64(nRoutes))

	err = db.QueryRow(`SELECT COUNT(*) FROM route_certificates`).Scan(&count)
	c.Assert(err, IsNil)
	c.Assert(count, Equals, int64(nRoutes-1)) // the last route doesn't have a cert
}
