package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// TestTheTemplateNameNamesTheMigrationFilesOnDisk: Open's template is named by the migration files
// as they are on disk, so a tree whose files differ in a byte, or that holds a file more or less,
// never clones a template another tree made. The row digests every file under ../migrations as
// store.MigrationsDigest digests the embedded ones (its path, migrations/<name>, a zero byte, its
// length, a zero byte and its bytes, in path order) and wants the same digest: an embed that left
// a file out, or a digest that skipped one, fails here. It wants templateForm to hold the text of
// each step migrateWithThisTree runs outside the migrations, embeddingsStatistics (#6050), so a
// change to that step's statement moves the template's name. It then wants the name Open's
// template takes for a login to be the scheme's prefix, the first 16 hex of the SHA-256 of that
// digest, a zero byte and templateForm, "_", and the first 12 hex of the SHA-256 of the login. It
// needs no database.
func TestTheTemplateNameNamesTheMigrationFilesOnDisk(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "migrations")
	files := sha256.New()
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(files, "%s\x00%d\x00", "migrations/"+filepath.ToSlash(relative), len(data))
		files.Write(data)
		return nil
	}); err != nil {
		t.Fatalf("read the migration files under %s: %v", root, err)
	}
	onDisk := hex.EncodeToString(files.Sum(nil))
	embedded, err := store.MigrationsDigest()
	if err != nil {
		t.Fatal(err)
	}
	if embedded != onDisk {
		t.Fatalf("store.MigrationsDigest() is %s, and the files under %s digest to %s: the embedded set is not the files on disk", embedded, root, onDisk)
	}
	if !strings.Contains(templateForm, embeddingsStatistics) {
		t.Fatalf("templateForm %q does not hold the step migrateWithThisTree runs outside the migrations, %q: a template built with another statement would keep the same name", templateForm, embeddingsStatistics)
	}

	digest, err := openSet.digest()
	if err != nil {
		t.Fatal(err)
	}
	const login = "a login's name, which may hold any character"
	form := sha256.Sum256([]byte(onDisk + "\x00" + templateForm))
	key := sha256.Sum256([]byte(login))
	want := "dispatch_shared_v1_" + hex.EncodeToString(form[:])[:16] + "_" + hex.EncodeToString(key[:])[:12]
	if got := openSet.scheme.templateName(digest, login); got != want {
		t.Fatalf("Open's template for the login %q is named %s; want %s, from the files on disk, templateForm and the login", login, got, want)
	}
}

// TestOpenHandsATestOnlyItsOwnClone: Open hands a test a store on a database of the test's own, a
// clone named dispatch_test_…, never the template that every test process of the server and login
// clones, and the template refuses every connection, so no test reaches it through a URL it builds
// from the server's. The row reads the database its store is on, then connects to the template as
// a test could, and wants Postgres's refusal of a database that allows no connections (SQLSTATE
// 55000).
func TestOpenHandsATestOnlyItsOwnClone(t *testing.T) {
	t.Parallel()
	database := Open(t)
	ctx := context.Background()
	var current string
	if err := database.Pool.QueryRow(ctx, "select current_database()").Scan(&current); err != nil {
		t.Fatalf("read the database Open's store is on: %v", err)
	}
	templates.Lock()
	template := templates.byKey[openKey].name
	templates.Unlock()
	if !strings.HasPrefix(current, "dispatch_test_") || current == template {
		t.Fatalf("Open's store is on %s; want a clone of the test's own, dispatch_test_…, not the template %s", current, template)
	}
	conn, err := pgx.Connect(ctx, testdb.WithConnectDeadline(t, databaseURL(shared.server, template)))
	if err == nil {
		_ = conn.Close(ctx)
		t.Fatalf("a connection to the template %s was let in; want it refused", template)
	}
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "55000" {
		t.Fatalf("a connection to the template %s failed on %v; want Postgres's refusal of a database that allows no connections (SQLSTATE 55000)", template, err)
	}
	t.Logf("Open's store is on %s, and a connection to the template %s was refused: %s", current, template, pgError.Message)
}
