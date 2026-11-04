package console

import (
	"slices"
	"testing"
)

func TestLayoutChoice(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		w, h int
		want layoutKind
	}{
		{160, 45, layoutWide},
		{120, 36, layoutWide},
		{119, 40, layoutCompact},
		{120, 35, layoutCompact},
		{80, 24, layoutCompact},
		{79, 24, layoutTooSmall},
		{80, 23, layoutTooSmall},
	} {
		if got := chooseLayout(row.w, row.h).kind; got != row.want {
			t.Errorf("chooseLayout(%d, %d) = %v, want %v", row.w, row.h, got, row.want)
		}
	}

	wide := chooseLayout(160, 45)
	if wide.Hardware.H != hardwareWide || wide.Right.W != rightWidth || wide.Events.H < minEvents {
		t.Errorf("the wide layout's boxes are %+v", wide)
	}

	if wide.Instances.H+wide.Events.H != wide.H-headerRows-hardwareWide-alertRows-keyRows {
		t.Errorf("the wide layout's rows do not add up: %+v", wide)
	}

	if wide.Keys.Y != wide.H-1 || wide.Alert.Y != wide.H-2 {
		t.Errorf("the wide layout's alert or key line is misplaced: %+v", wide)
	}

	compact := chooseLayout(80, 24)
	if compact.Right.H != 0 || compact.Events.H != 0 || compact.Hardware.H != hardwareRows {
		t.Errorf("the compact layout keeps a wide box: %+v", compact)
	}

	if compact.Log.H != 1 || compact.Keys.Y != compact.H-1 {
		t.Errorf("the compact layout's last log or key line is misplaced: %+v", compact)
	}

	if compact.Instances.H < minTable {
		t.Errorf("the compact layout's table is %d rows", compact.Instances.H)
	}
}

// columnIDs names the columns a layout shows.
func columnIDs(w int, kind layoutKind) []colID {
	cols, _ := tableColumns(w, kind)
	ids := make([]colID, 0, len(cols))

	for _, c := range cols {
		ids = append(ids, c.id)
	}

	return ids
}

func TestColumnDrop(t *testing.T) {
	t.Parallel()

	all := []colID{colName, colType, colState, colCPU, colRAM, colDisk, colIP, colUp}

	if got := columnIDs(112, layoutWide); !slices.Equal(got, all) {
		t.Errorf("a wide table shows %v, want %v", got, all)
	}

	if _, bars := tableColumns(112, layoutWide); !bars {
		t.Error("a wide table has no mini-bars")
	}

	// UP, then DISK, then the mini-bars drop from the right.
	if got, want := columnIDs(72, layoutWide), []colID{colName, colType, colState, colCPU, colRAM, colIP}; !slices.Equal(got, want) {
		t.Errorf("a 72-wide table shows %v, want %v", got, want)
	}

	if _, bars := tableColumns(72, layoutWide); bars {
		t.Error("a 72-wide table still has mini-bars")
	}

	if got, want := columnIDs(80, layoutCompact), []colID{colName, colType, colState, colCPU, colRAM, colIP}; !slices.Equal(got, want) {
		t.Errorf("a compact table shows %v, want %v", got, want)
	}

	if got, want := columnIDs(44, layoutCompact), []colID{colName, colType, colState, colIP}; !slices.Equal(got, want) {
		t.Errorf("a 44-wide table shows %v, want %v", got, want)
	}

	cols, _ := tableColumns(160, layoutWide)
	if cols[0].w < nameMinWidth || cols[0].w > nameMaxWidth {
		t.Errorf("the name column is %d wide", cols[0].w)
	}
}

// TestWide120ShowsAllColumns checks that the smallest wide layout keeps every column and the bars.
func TestWide120ShowsAllColumns(t *testing.T) {
	t.Parallel()

	wide := chooseLayout(120, 36)
	table := wide.Instances.W - gutter
	all := []colID{colName, colType, colState, colCPU, colRAM, colDisk, colIP, colUp}

	if got := columnIDs(table, layoutWide); !slices.Equal(got, all) {
		t.Errorf("a 120-wide layout shows %v, want %v", got, all)
	}

	cols, bars := tableColumns(table, layoutWide)
	if !bars {
		t.Error("the 120-wide table has no mini-bars")
	}

	if cols[0].w < nameMinWidth {
		t.Errorf("the name column is %d wide, want at least %d", cols[0].w, nameMinWidth)
	}
}

func TestGoldenLayouts(t *testing.T) {
	t.Parallel()

	for _, size := range []struct {
		w, h int
		name string
	}{
		{80, 25, "normal-80x25.txt"},
		{120, 36, "normal-120x36.txt"},
		{160, 45, "normal-160x45.txt"},
		{60, 20, "small-60x20.txt"},
	} {
		t.Run(size.name, func(t *testing.T) {
			t.Parallel()

			golden(t, size.name, draw(t, tracerSnapshot(), size.w, size.h))
		})
	}
}
