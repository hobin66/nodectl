package service

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"
)

const (
	airportAutoUpdateEnabledKey  = "airport_auto_update_enabled"
	airportAutoUpdateIntervalKey = "airport_auto_update_interval_minutes"
	defaultAirportAutoUpdateMins = 360
	maxAirportAutoUpdateMins     = 10080
)

var (
	airportSubscriptionAutoUpdateOnce  sync.Once
	airportSubscriptionAutoUpdateRunMu sync.Mutex
	airportAutoAttempts                = &airportAutoAttemptState{lastAttemptBySub: make(map[string]time.Time)}
)

type airportAutoAttemptState struct {
	mu               sync.Mutex
	lastAttemptBySub map[string]time.Time
}

func airportAutoUpdateDue(updatedAt, lastAutoAttempt, now time.Time, interval time.Duration) bool {
	lastActivity := updatedAt
	if lastAutoAttempt.After(lastActivity) {
		lastActivity = lastAutoAttempt
	}
	return lastActivity.IsZero() || !now.Before(lastActivity.Add(interval))
}

func (s *airportAutoAttemptState) isDue(sub database.AirportSub, now time.Time, interval time.Duration) bool {
	s.mu.Lock()
	lastAutoAttempt := s.lastAttemptBySub[sub.ID]
	s.mu.Unlock()
	return airportAutoUpdateDue(sub.UpdatedAt, lastAutoAttempt, now, interval)
}

func (s *airportAutoAttemptState) beginAttempt(sub database.AirportSub, now time.Time, interval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !airportAutoUpdateDue(sub.UpdatedAt, s.lastAttemptBySub[sub.ID], now, interval) {
		return false
	}
	if s.lastAttemptBySub == nil {
		s.lastAttemptBySub = make(map[string]time.Time)
	}
	s.lastAttemptBySub[sub.ID] = now
	return true
}

// StartAirportSubscriptionAutoUpdate starts the serialized subscription sync loop once.
func StartAirportSubscriptionAutoUpdate() {
	airportSubscriptionAutoUpdateOnce.Do(func() {
		go func() {
			time.Sleep(time.Minute)
			runAirportSubscriptionAutoUpdate()

			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				runAirportSubscriptionAutoUpdate()
			}
		}()
	})
}

func runAirportSubscriptionAutoUpdate() {
	airportSubscriptionAutoUpdateRunMu.Lock()
	defer airportSubscriptionAutoUpdateRunMu.Unlock()

	if database.DB == nil {
		logger.Log.Warn("机场订阅自动同步跳过：数据库未初始化")
		return
	}
	if !isSysConfigEnabled(airportAutoUpdateEnabledKey) {
		return
	}

	intervalMinutes := airportSubscriptionAutoUpdateInterval()
	interval := time.Duration(intervalMinutes) * time.Minute
	var subs []database.AirportSub
	if err := database.DB.Find(&subs).Error; err != nil {
		logger.Log.Error("读取机场订阅列表失败", "error", err)
		return
	}

	syncDueAirportSubscriptions(subs, interval, airportAutoAttempts, time.Now, SyncAirportSubscription, time.Sleep)
}

// syncDueAirportSubscriptions records each automatic attempt before syncing, including failed attempts.
func syncDueAirportSubscriptions(
	subs []database.AirportSub,
	interval time.Duration,
	attempts *airportAutoAttemptState,
	now func() time.Time,
	syncSub func(string) error,
	sleep func(time.Duration),
) (attempted, success, failed int) {
	checkedAt := now()
	due := make([]database.AirportSub, 0, len(subs))
	for _, sub := range subs {
		if attempts.isDue(sub, checkedAt, interval) {
			due = append(due, sub)
		}
	}
	if len(due) == 0 {
		return 0, 0, 0
	}

	logger.Log.Info("开始自动同步机场订阅", "due", len(due), "interval_minutes", int(interval/time.Minute))
	for i, sub := range due {
		if !attempts.beginAttempt(sub, now(), interval) {
			continue
		}
		attempted++
		if err := syncSub(sub.ID); err != nil {
			failed++
			logger.Log.Error("机场订阅自动同步失败", "id", sub.ID, "name", sub.Name, "error", err)
		} else {
			success++
			logger.Log.Info("机场订阅自动同步成功", "id", sub.ID, "name", sub.Name)
		}
		if i < len(due)-1 {
			sleep(3 * time.Second)
		}
	}
	logger.Log.Info("机场订阅自动同步完成", "success", success, "failed", failed)
	return attempted, success, failed
}

func airportSubscriptionAutoUpdateInterval() int {
	minutes, err := strconv.Atoi(strings.TrimSpace(getSysConfigValue(airportAutoUpdateIntervalKey)))
	if err != nil || minutes < 1 || minutes > maxAirportAutoUpdateMins {
		return defaultAirportAutoUpdateMins
	}
	return minutes
}
