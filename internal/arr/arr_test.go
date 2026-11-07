package arr

import "testing"

func TestReleaseGrab(t *testing.T) {
	grab := func(id int, dl string) HistoryRecord {
		return HistoryRecord{ID: id, EventType: EventGrabbed, DownloadID: dl}
	}
	imp := func(id int, dl string) HistoryRecord {
		return HistoryRecord{ID: id, EventType: EventImported, DownloadID: dl}
	}
	failed := HistoryRecord{ID: 50, EventType: "downloadFailed", DownloadID: "B"}

	tests := []struct {
		name    string
		records []HistoryRecord
		want    int
		ok      bool
	}{
		{"matches the import", []HistoryRecord{imp(3, "A"), grab(2, "A"), failed, grab(1, "B")}, 2, true},
		{"newer grab never imported", []HistoryRecord{grab(4, "C"), imp(3, "A"), grab(2, "A")}, 2, true},
		{"no matching grab: newest grab", []HistoryRecord{imp(3, "A"), grab(2, "B")}, 2, true},
		{"import without download id: newest grab", []HistoryRecord{imp(3, ""), grab(2, "B"), grab(1, "C")}, 2, true},
		{"no grabs", []HistoryRecord{imp(3, "")}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, tt := range tests {
		g, ok := ReleaseGrab(tt.records)
		if ok != tt.ok || g.ID != tt.want {
			t.Errorf("%s: got %d, %v; want %d, %v", tt.name, g.ID, ok, tt.want, tt.ok)
		}
	}
}
