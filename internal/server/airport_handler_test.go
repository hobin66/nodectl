package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestAirportEditPreservesLastSuccessfulSyncTime(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "airport-edit.db")), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := db.AutoMigrate(&database.AirportSub{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	oldDB, oldLog := database.DB, logger.Log
	database.DB = db
	logger.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() {
		database.DB = oldDB
		logger.Log = oldLog
	})

	unsynced := database.AirportSub{Name: "New Airport", URL: "https://new.example/sub"}
	if err := db.Create(&unsynced).Error; err != nil {
		t.Fatalf("create never-synced subscription: %v", err)
	}
	var newSub database.AirportSub
	if err := db.First(&newSub, "id = ?", unsynced.ID).Error; err != nil {
		t.Fatalf("read never-synced subscription: %v", err)
	}
	if !newSub.UpdatedAt.IsZero() {
		t.Fatalf("new subscription has a successful sync time before syncing: %v", newSub.UpdatedAt)
	}

	now := time.Now()
	lastSuccess := now.Add(-7 * time.Hour).UTC().Truncate(time.Second)
	sub := database.AirportSub{ID: "edit-old-sub", Name: "Old Airport", URL: "https://old.example/sub", UpdatedAt: lastSuccess}
	if err := db.Create(&sub).Error; err != nil {
		t.Fatalf("create previously synced subscription: %v", err)
	}
	for _, tc := range []struct {
		name     string
		body     string
		wantName string
		wantURL  string
	}{
		{name: "name", body: `{"id":"edit-old-sub","name":"Renamed Airport"}`, wantName: "Renamed Airport", wantURL: "https://old.example/sub"},
		{name: "url", body: `{"id":"edit-old-sub","url":"https://changed.example/sub"}`, wantName: "Renamed Airport", wantURL: "https://changed.example/sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/airport/edit", strings.NewReader(tc.body))
			response := httptest.NewRecorder()
			apiAirportEdit(response, request)
			var result struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatalf("parse edit response: %v", err)
			}
			if result.Status != "success" {
				t.Fatalf("edit response = %s", response.Body.String())
			}
			var got database.AirportSub
			if err := db.First(&got, "id = ?", sub.ID).Error; err != nil {
				t.Fatalf("read edited subscription: %v", err)
			}
			if got.Name != tc.wantName || got.URL != tc.wantURL {
				t.Fatalf("edited name/url = %q / %q", got.Name, got.URL)
			}
			if !got.UpdatedAt.Equal(lastSuccess) {
				t.Fatalf("editing %s changed last successful sync from %v to %v", tc.name, lastSuccess, got.UpdatedAt)
			}
			if now.Before(got.UpdatedAt.Add(6 * time.Hour)) {
				t.Fatalf("editing %s incorrectly postponed the six-hour scheduler due time", tc.name)
			}
		})
	}
}
