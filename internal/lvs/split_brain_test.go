// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 tianhao

package lvs

import (
	"testing"
	"time"
)

func TestDetectSplitBrain(t *testing.T) {
	cases := []struct {
		name    string
		holders []directorHolder
		want    bool
		wantN   int
	}{
		{
			name:    "no directors",
			holders: nil,
			want:    false,
			wantN:   0,
		},
		{
			name:    "single holder (normal master)",
			holders: []directorHolder{{ID: 1, HoldingVIP: true, VIP: "10.0.0.10"}},
			want:    false,
			wantN:   1,
		},
		{
			name: "two holders -> split brain",
			holders: []directorHolder{
				{ID: 1, HoldingVIP: true, VIP: "10.0.0.10"},
				{ID: 2, HoldingVIP: true, VIP: "10.0.0.10"},
			},
			want:  true,
			wantN: 2,
		},
		{
			name: "three holders -> split brain",
			holders: []directorHolder{
				{ID: 1, HoldingVIP: true, VIP: "10.0.0.10"},
				{ID: 2, HoldingVIP: true, VIP: "10.0.0.10"},
				{ID: 3, HoldingVIP: true, VIP: "10.0.0.10"},
			},
			want:  true,
			wantN: 3,
		},
		{
			name: "one of two holds -> no split brain",
			holders: []directorHolder{
				{ID: 1, HoldingVIP: true, VIP: "10.0.0.10"},
				{ID: 2, HoldingVIP: false, VIP: "10.0.0.10"},
			},
			want:  false,
			wantN: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			detected, active, _ := DetectSplitBrain(c.holders, time.Second)
			if detected != c.want {
				t.Errorf("detected=%v want %v", detected, c.want)
			}
			if len(active) != c.wantN {
				t.Errorf("active len=%d want %d (active=%v)", len(active), c.wantN, active)
			}
		})
	}
}
