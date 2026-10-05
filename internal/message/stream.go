package message

// StreamPolicy is administrator-owned. Clients select a name, never a weight.
type StreamPolicy struct {
	Name   string           `json:"name"`
	Weight int              `json:"weight"`
	Budget SubmissionBudget `json:"budget"`
}

func CanonicalStream(name string) (string, error) {
	switch name {
	case "", "normal":
		return "normal", nil
	case "otp", "security", "receipt":
		return name, nil
	default:
		return "", ErrInvalidSubmission
	}
}
func CanonicalStreams(in []StreamPolicy) ([]StreamPolicy, error) {
	if len(in) > 4 {
		return nil, ErrInvalidSubmission
	}
	out := append([]StreamPolicy{}, in...)
	seen := map[string]bool{}
	for _, p := range out {
		name, err := CanonicalStream(p.Name)
		if err != nil || p.Name == "" || seen[name] || p.Weight < 1 || p.Weight > 8 || !p.Budget.Valid() {
			return nil, ErrInvalidSubmission
		}
		seen[name] = true
	}
	if !seen["normal"] {
		out = append(out, StreamPolicy{Name: "normal", Weight: 1})
	}
	return out, nil
}
