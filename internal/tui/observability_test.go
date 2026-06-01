package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"proxylm/internal/ipc"
)

func TestServerColorByIndex_DistinctAndWraps(t *testing.T) {
	// Первые len(serverPalette) индексов дают различные цвета.
	seen := make(map[lipgloss.TerminalColor]int)
	for i := range serverPalette {
		seen[ServerColorByIndex(i).GetForeground()]++
	}
	if len(seen) != len(serverPalette) {
		t.Fatalf("expected %d distinct colors, got %d", len(serverPalette), len(seen))
	}
	// Индекс len совпадает по цвету с индексом 0 (wrap-around по модулю).
	if ServerColorByIndex(0).GetForeground() != ServerColorByIndex(len(serverPalette)).GetForeground() {
		t.Fatalf("expected wrap-around at len(serverPalette)")
	}
	// Отрицательный индекс безопасно деградирует в StyleDim.
	if ServerColorByIndex(-1).GetForeground() != StyleDim.GetForeground() {
		t.Fatalf("expected StyleDim for negative index")
	}
}

func TestApplyServerList_SortsByPriority(t *testing.T) {
	m := &model{flashMap: map[string]flashEntry{}}
	m.applyServerList([]ipc.ServerState{
		{Name: "cloud", Priority: 900},
		{Name: "local-b", Priority: 100},
		{Name: "local-a", Priority: 100},
		{Name: "rack", Priority: 200},
	})
	got := make([]string, len(m.servers))
	for i, s := range m.servers {
		got[i] = s.Name
	}
	want := []string{"local-a", "local-b", "rack", "cloud"} // priority asc, tiebreak by name
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestCurrentModelUnloaded(t *testing.T) {
	tests := []struct {
		name string
		s    ipc.ServerState
		want bool
	}{
		{
			name: "not probed → false",
			s:    ipc.ServerState{CurrentModel: "m1", LoadedModelsProbed: false},
			want: false,
		},
		{
			name: "idle (empty current) → false",
			s:    ipc.ServerState{CurrentModel: "", LoadedModelsProbed: true},
			want: false,
		},
		{
			name: "current in memory → false",
			s:    ipc.ServerState{CurrentModel: "m1", LoadedModelsProbed: true, LoadedModels: []string{"m1", "m2"}},
			want: false,
		},
		{
			name: "current NOT in memory → true",
			s:    ipc.ServerState{CurrentModel: "m1", LoadedModelsProbed: true, LoadedModels: []string{"m2"}},
			want: true,
		},
		{
			name: "probed but memory empty → true",
			s:    ipc.ServerState{CurrentModel: "m1", LoadedModelsProbed: true, LoadedModels: nil},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := currentModelUnloaded(tt.s); got != tt.want {
				t.Fatalf("currentModelUnloaded = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadedModelsText(t *testing.T) {
	tests := []struct {
		name string
		s    ipc.ServerState
		want string
	}{
		{"not probed", ipc.ServerState{LoadedModelsProbed: false}, "n/a"},
		{"probed empty", ipc.ServerState{LoadedModelsProbed: true}, "— (none)"},
		{"probed list", ipc.ServerState{LoadedModelsProbed: true, LoadedModels: []string{"a", "b"}}, "a, b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loadedModelsText(tt.s); got != tt.want {
				t.Fatalf("loadedModelsText = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPulseStyle_CyclesAndNonNegative(t *testing.T) {
	// Фаза по модулю длины кадров; отрицательная фаза не паникует.
	for phase := -3; phase < len(serverPulseFrames)*2; phase++ {
		_ = pulseStyle(phase) // не должно паниковать / выходить за границы
	}
	if pulseStyle(0).GetForeground() != serverPulseFrames[0].GetForeground() {
		t.Fatalf("phase 0 should map to frame 0")
	}
	if pulseStyle(len(serverPulseFrames)).GetForeground() != serverPulseFrames[0].GetForeground() {
		t.Fatalf("phase len should wrap to frame 0")
	}
}
