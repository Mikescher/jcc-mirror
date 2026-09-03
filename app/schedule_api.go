package app

import (
	"net/http"
	"time"

	"blackforestbytes.com/jcc-mirror/schedule"
)

// ScheduleView is the two grids as the dashboard draws them: the rules in the
// form they are edited and stored in, and the 168 cells they expand to. Both
// halves are sent because the colours say the shape at a glance and the rules say
// the exact numbers, and neither answers the other's question (DESIGN.md §6).
type ScheduleView struct {
	Now      ScheduleStatus `json:"now"`
	Transfer GridView       `json:"transfer"`
	Scan     GridView       `json:"scan"`
}

// GridView is one 7x24 schedule. Cells are indexed [weekday][hour] with Monday
// first, which is how the week reads here and not how time.Weekday numbers one.
type GridView struct {
	Rules string   `json:"rules"`
	Cells [][]Cell `json:"cells"`
	Days  []string `json:"days"`
}

// Cell is one weekday-hour: whether bytes may move and how fast, with 0 meaning
// no cap rather than no bandwidth.
type Cell struct {
	Open  bool  `json:"open"`
	Limit int64 `json:"limit"`
}

func (a *App) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	transfer, scan := a.Schedules()
	writeJSON(w, http.StatusOK, ScheduleView{
		Now:      a.ScheduleStatus(),
		Transfer: gridOf(transfer),
		Scan:     gridOf(scan),
	})
}

var mondayFirstDays = []time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
	time.Friday, time.Saturday, time.Sunday,
}

func gridOf(s schedule.Schedule) GridView {
	g := GridView{Rules: s.String()}
	for _, day := range mondayFirstDays {
		g.Days = append(g.Days, day.String())
		row := make([]Cell, schedule.Hours)
		for hour := range schedule.Hours {
			cell := s.Cell(day, hour)
			row[hour] = Cell{Open: cell.Open, Limit: cell.Limit}
		}
		g.Cells = append(g.Cells, row)
	}
	return g
}
