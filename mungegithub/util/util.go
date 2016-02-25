package util

func NormalizeStringSlice(in []string) []string {
	out := []string{}
	for _, s := range in {
		if len(s) != 0 {
			out = append(out, s)
		}
	}
	return out
}
