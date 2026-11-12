package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

type SourceAccess struct{ GateOrigin, GateToken, AccountOrigin, AccountToken string }

var sourceAccessToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s SourceAccess) Valid() bool {
	if !sourceAccessToken.MatchString(s.GateToken) || !sourceAccessToken.MatchString(s.AccountToken) || s.GateToken == s.AccountToken {
		return false
	}
	for _, origin := range []string{s.GateOrigin, s.AccountOrigin} {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(origin, "?#") {
			return false
		}
	}
	return true
}
func loadSourceAccess() (*SourceAccess, error) {
	s := SourceAccess{GateOrigin: os.Getenv("ASSET_SOURCE_GATE_ORIGIN"), GateToken: os.Getenv("ASSET_SOURCE_GATE_TOKEN"), AccountOrigin: os.Getenv("ASSET_ACCOUNT_ORIGIN"), AccountToken: os.Getenv("ASSET_ACCOUNT_TOKEN")}
	if s == (SourceAccess{}) {
		return nil, nil
	}
	if !s.Valid() {
		return nil, fmt.Errorf("invalid ASSET_SOURCE_GATE / ASSET_ACCOUNT configuration")
	}
	return &s, nil
}
