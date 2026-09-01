package ports

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	testCases := map[string]struct {
		args       []string
		namedPorts map[string]uint16
		want       []Pair
		wantErr    bool
	}{
		"remote only": {
			args: []string{"80"},
			want: []Pair{{Local: 80, Remote: 80}},
		},
		"local and remote": {
			args: []string{"8080:80"},
			want: []Pair{{Local: 8080, Remote: 80}},
		},
		"ephemeral local": {
			args: []string{":80"},
			want: []Pair{{Local: 0, Remote: 80}},
		},
		"multiple args": {
			args: []string{"8080:80", "9090:90"},
			want: []Pair{{Local: 8080, Remote: 80}, {Local: 9090, Remote: 90}},
		},
		"explicit tcp": {
			args: []string{"8080:80@tcp"},
			want: []Pair{{Local: 8080, Remote: 80}},
		},
		"udp rejected": {
			args:    []string{"53:53@udp"},
			wantErr: true,
		},
		"unknown protocol": {
			args:    []string{"80@sctp"},
			wantErr: true,
		},
		"named remote port": {
			args:       []string{"8080:http"},
			namedPorts: map[string]uint16{"http": 80},
			want:       []Pair{{Local: 8080, Remote: 80}},
		},
		"named port not found": {
			args:    []string{"8080:http"},
			wantErr: true,
		},
		"too many colons": {
			args:    []string{"1:2:3"},
			wantErr: true,
		},
		"invalid local port": {
			args:    []string{"foo:80"},
			wantErr: true,
		},
		"port out of range": {
			args:    []string{"70000"},
			wantErr: true,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(tc.args, tc.namedPorts)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
