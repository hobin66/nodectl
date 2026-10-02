package service

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func airportDeleteTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "airport-delete.db")), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := db.AutoMigrate(&database.AirportSub{}, &database.AirportNode{}, &database.SysConfig{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	oldDB, oldLog := database.DB, logger.Log
	database.DB = db
	logger.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() {
		database.DB = oldDB
		logger.Log = oldLog
	})
	return db
}

func waitAirportDeleteResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for airport deletion")
		return nil
	}
}

func waitAirportAutoSyncResult(t *testing.T, done <-chan [3]int) [3]int {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for automatic airport sync")
		return [3]int{}
	}
}

func startAirportDeleteAutoSync(sub database.AirportSub, done chan<- [3]int) {
	attempts := &airportAutoAttemptState{}
	attempted, success, failed := syncDueAirportSubscriptions(
		[]database.AirportSub{sub}, 6*time.Hour, attempts, time.Now,
		SyncAirportSubscription, func(time.Duration) {},
	)
	done <- [3]int{attempted, success, failed}
}

func assertAirportDeletedWithoutOrphans(t *testing.T, db *gorm.DB, subID string) {
	t.Helper()
	var subs, nodes int64
	if err := db.Model(&database.AirportSub{}).Where("id = ?", subID).Count(&subs).Error; err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if err := db.Model(&database.AirportNode{}).Where("sub_id = ?", subID).Count(&nodes).Error; err != nil {
		t.Fatalf("count airport nodes: %v", err)
	}
	if subs != 0 || nodes != 0 {
		t.Fatalf("deleted subscription has %d subscription rows and %d orphan nodes", subs, nodes)
	}
}

func TestAirportAutoSyncThenDeleteIsSerialized(t *testing.T) {
	db := airportDeleteTestDB(t)
	syncReached := make(chan struct{})
	releaseSync := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSync) }) }

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "v2rayN/6.31" {
			return
		}
		close(syncReached)
		<-releaseSync
		_, _ = io.WriteString(w, "proxies:\n  - name: Sync Node\n    type: ss\n    server: node.example\n    port: 443\n    cipher: aes-128-gcm\n    password: secret\n")
	}))
	defer func() {
		release()
		source.Close()
	}()

	sub := database.AirportSub{ID: "sync-first", Name: "Airport", URL: source.URL, UpdatedAt: time.Now().Add(-7 * time.Hour)}
	if err := db.Create(&sub).Error; err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	autoDone := make(chan [3]int, 1)
	go startAirportDeleteAutoSync(sub, autoDone)
	select {
	case <-syncReached:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic sync did not reach the subscription source")
	}

	deleteStarted := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		close(deleteStarted)
		deleteDone <- DeleteAirportSubscription(sub.ID)
	}()
	<-deleteStarted
	select {
	case err := <-deleteDone:
		t.Fatalf("deletion completed during automatic sync: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	if got := waitAirportAutoSyncResult(t, autoDone); got != [3]int{1, 1, 0} {
		t.Fatalf("automatic sync result = %v, want [1 1 0]", got)
	}
	if err := waitAirportDeleteResult(t, deleteDone); err != nil {
		t.Fatalf("delete subscription: %v", err)
	}
	assertAirportDeletedWithoutOrphans(t, db, sub.ID)
}

func TestAirportDeleteThenAutoSyncFindsNoSubscription(t *testing.T) {
	db := airportDeleteTestDB(t)
	sourceReached := make(chan struct{}, 1)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sourceReached <- struct{}{}:
		default:
		}
	}))
	defer source.Close()

	sub := database.AirportSub{ID: "delete-first", Name: "Airport", URL: source.URL, UpdatedAt: time.Now().Add(-7 * time.Hour)}
	if err := db.Create(&sub).Error; err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if err := db.Create(&database.AirportNode{SubID: sub.ID, Name: "Old Node"}).Error; err != nil {
		t.Fatalf("create old node: %v", err)
	}

	deleteReached := make(chan struct{})
	releaseDelete := make(chan struct{})
	var reachedOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDelete) }) }
	defer release()
	if err := db.Callback().Delete().Before("gorm:delete").Register("test:pause-airport-delete", func(tx *gorm.DB) {
		if tx.Statement.Table == "airport_nodes" {
			reachedOnce.Do(func() { close(deleteReached) })
			<-releaseDelete
		}
	}); err != nil {
		t.Fatalf("register delete callback: %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- DeleteAirportSubscription(sub.ID) }()
	select {
	case <-deleteReached:
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not reach the database")
	}

	autoDone := make(chan [3]int, 1)
	go startAirportDeleteAutoSync(sub, autoDone)
	select {
	case <-sourceReached:
		t.Fatal("automatic sync contacted the source while deletion held the subscription lock")
	case <-time.After(100 * time.Millisecond):
	}

	release()
	if err := waitAirportDeleteResult(t, deleteDone); err != nil {
		t.Fatalf("delete subscription: %v", err)
	}
	if got := waitAirportAutoSyncResult(t, autoDone); got != [3]int{1, 0, 1} {
		t.Fatalf("automatic sync result = %v, want [1 0 1]", got)
	}
	select {
	case <-sourceReached:
		t.Fatal("automatic sync contacted the source after deletion")
	default:
	}
	assertAirportDeletedWithoutOrphans(t, db, sub.ID)
}
