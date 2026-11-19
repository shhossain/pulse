package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pulse/internal/model"
)

func TestOpenPathWithURISyntax(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd #?% name")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pulse.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &model.Run{ID: "r_1", Job: "j", Seq: 1, Status: model.Running, UpdatedAt: time.Now()}
	if err := st.Save(r, nil); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not at the intended path: %v", err)
	}
}

func TestMigrationsAndNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pulse.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	st.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != SchemaVersion() {
		t.Fatalf("user_version = %d", v)
	}
	st.db.Exec(`PRAGMA user_version = 99`)
	st.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer Pulse") {
		t.Fatalf("newer schema: %v", err)
	}
}

func TestPruneKeepsRecentAndRunning(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "pulse.db"))
	defer st.Close()
	old := time.Now().Add(-40 * 24 * time.Hour)
	st.Save(&model.Run{ID: "r_old", Job: "j", Seq: 1, Status: model.Done, UpdatedAt: old}, []model.Point{{Run: "r_old", Key: "loss", TS: old, V: 1}})
	st.Save(&model.Run{ID: "r_live", Job: "j", Seq: 2, Status: model.Running, UpdatedAt: old}, nil)
	st.Save(&model.Run{ID: "r_new", Job: "j", Seq: 3, Status: model.Done, UpdatedAt: time.Now()}, nil)
	ids, err := st.Prune(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil || len(ids) != 1 || ids[0] != "r_old" {
		t.Fatalf("pruned %v %v", ids, err)
	}
	if pts, _ := st.Series("r_old", "loss"); len(pts) != 0 {
		t.Fatal("series left behind")
	}
}

func TestPruneKeepsEachJobsLatestRun(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "pulse.db"))
	defer st.Close()
	old := time.Now().Add(-60 * 24 * time.Hour)
	st.Save(&model.Run{ID: "r_6", Job: "monthly", Seq: 6, Status: model.Done, UpdatedAt: old}, nil)
	st.Save(&model.Run{ID: "r_7", Job: "monthly", Seq: 7, Status: model.Done, UpdatedAt: old}, nil)
	ids, _ := st.Prune(time.Now().Add(-30 * 24 * time.Hour))
	if len(ids) != 1 || ids[0] != "r_6" {
		t.Fatalf("pruned %v", ids)
	}
	if n, _ := st.MaxSeq("monthly"); n != 7 {
		t.Fatalf("run counter lost: %d", n)
	}
}
