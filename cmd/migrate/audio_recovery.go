package main

import (
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type audioRecovery struct {
	packageID, lastClaim string
	apply                bool
}

func parseAudioRecovery(args []string) (audioRecovery, bool, error) {
	var result audioRecovery
	handled := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "--recording-audio-retry-") || strings.HasPrefix(arg, "--recording-audio-last-claim") {
			handled = true
		}
	}
	if !handled {
		return result, false, nil
	}
	f := flag.NewFlagSet("audio-recovery", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	plan := f.String("recording-audio-retry-plan", "", "read-only cost and attempt preview for one failed package")
	apply := f.String("recording-audio-retry-apply", "", "allow exactly one additional attempt for the approved package")
	claim := f.String("recording-audio-last-claim", "", "exact last failed claim from the approved preview; apply only")
	if err := f.Parse(args); err != nil {
		return result, true, err
	}
	hasPlan, hasApply, hasClaim := false, false, false
	f.Visit(func(v *flag.Flag) {
		switch v.Name {
		case "recording-audio-retry-plan":
			hasPlan = true
		case "recording-audio-retry-apply":
			hasApply = true
		case "recording-audio-last-claim":
			hasClaim = true
		}
	})
	valid := regexp.MustCompile(`^[a-f0-9]{32}$`)
	if f.NArg() != 0 || hasPlan == hasApply || hasPlan && hasClaim || hasApply && (!hasClaim || !valid.MatchString(*claim)) || hasPlan && !valid.MatchString(*plan) || hasApply && !valid.MatchString(*apply) {
		return result, true, fmt.Errorf("select one exact package retry plan or apply; apply requires its last failed claim")
	}
	result = audioRecovery{packageID: *plan}
	if hasApply {
		result = audioRecovery{packageID: *apply, lastClaim: *claim, apply: true}
	}
	return result, true, nil
}
