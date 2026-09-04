package ports

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	testCases := map[string]struct {
		args       []string
		namedPorts map[string]NamedPort
		want       []Pair
		wantErr    bool
	}{
		"remote only": {
			args: []string{"80"},
			want: []Pair{{Local: 80, Remote: 80, Proto: "tcp"}},
		},
		"local and remote": {
			args: []string{"8080:80"},
			want: []Pair{{Local: 8080, Remote: 80, Proto: "tcp"}},
		},
		"ephemeral local": {
			args: []string{":80"},
			want: []Pair{{Local: 0, Remote: 80, Proto: "tcp"}},
		},
		"multiple args": {
			args: []string{"8080:80", "9090:90"},
			want: []Pair{{Local: 8080, Remote: 80, Proto: "tcp"}, {Local: 9090, Remote: 90, Proto: "tcp"}},
		},
		"explicit tcp": {
			args: []string{"8080:80@tcp"},
			want: []Pair{{Local: 8080, Remote: 80, Proto: "tcp"}},
		},
		"explicit udp": {
			args: []string{"10053:53@udp"},
			want: []Pair{{Local: 10053, Remote: 53, Proto: "udp"}},
		},
		"unknown protocol": {
			args:    []string{"80@sctp"},
			wantErr: true,
		},
		"named remote port": {
			args:       []string{"8080:http"},
			namedPorts: map[string]NamedPort{"http": {Port: 80, Proto: "tcp"}},
			want:       []Pair{{Local: 8080, Remote: 80, Proto: "tcp"}},
		},
		"named udp port infers protocol": {
			args:       []string{"10053:dns"},
			namedPorts: map[string]NamedPort{"dns": {Port: 53, Proto: "udp"}},
			want:       []Pair{{Local: 10053, Remote: 53, Proto: "udp"}},
		},
		"named port with matching explicit protocol": {
			args:       []string{"10053:dns@udp"},
			namedPorts: map[string]NamedPort{"dns": {Port: 53, Proto: "udp"}},
			want:       []Pair{{Local: 10053, Remote: 53, Proto: "udp"}},
		},
		"named port with conflicting protocol": {
			args:       []string{"10053:dns@tcp"},
			namedPorts: map[string]NamedPort{"dns": {Port: 53, Proto: "udp"}},
			wantErr:    true,
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
