package mullvad

import (
	"errors"
	"testing"
)

func TestSOCKS5Address(t *testing.T) {
	cases := []struct {
		name     string
		hostname string
		want     string
		wantErr  error
	}{
		{
			name:     "three-digit relay number",
			hostname: "al-tia-wg-003",
			want:     "al-tia-wg-socks5-003.relays.mullvad.net:1080",
		},
		{
			name:     "stockholm relay 001",
			hostname: "se-sto-wg-001",
			want:     "se-sto-wg-socks5-001.relays.mullvad.net:1080",
		},
		{
			name:     "amsterdam relay 001",
			hostname: "nl-ams-wg-001",
			want:     "nl-ams-wg-socks5-001.relays.mullvad.net:1080",
		},
		{
			name:     "us new york relay 301",
			hostname: "us-nyc-wg-301",
			want:     "us-nyc-wg-socks5-301.relays.mullvad.net:1080",
		},
		{
			name:     "trailing number with extra digits stays intact",
			hostname: "us-qas-wg-12345",
			want:     "us-qas-wg-socks5-12345.relays.mullvad.net:1080",
		},
		{
			name:     "missing trailing number is rejected",
			hostname: "se-sto-wg",
			wantErr:  ErrUnexpectedHostname,
		},
		{
			name:     "missing -wg- segment is rejected",
			hostname: "se-sto-001",
			wantErr:  ErrUnexpectedHostname,
		},
		{
			name:     "empty input is rejected",
			hostname: "",
			wantErr:  ErrUnexpectedHostname,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SOCKS5Address(tc.hostname)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil (result %q)", tc.wantErr, got)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected errors.Is(err, %v), got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
