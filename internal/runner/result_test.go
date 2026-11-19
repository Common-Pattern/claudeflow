package runner

import "testing"

func TestEndedCleanly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transcript string
		want       bool
	}{
		{"success", `{"type":"assistant"}` + "\n" + `{"type":"result","subtype":"success","is_error":false}` + "\n", true},
		{"no result: killed or crashed", `{"type":"assistant"}` + "\n", false},
		{"error result", `{"type":"result","subtype":"error_during_execution","is_error":true}`, false},
		{"success flagged as error", `{"type":"result","subtype":"success","is_error":true}`, false},
		{"the last result decides",
			`{"type":"result","subtype":"success","is_error":false}` + "\n" +
				`{"type":"result","subtype":"error_max_turns","is_error":true}`, false},
		{"stderr noise around it", "pnpm says hi\n" + `{"type":"result","subtype":"success","is_error":false}` + "\nbye", true},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EndedCleanly(tc.transcript); got != tc.want {
				t.Errorf("EndedCleanly = %v, want %v", got, tc.want)
			}
		})
	}
}
