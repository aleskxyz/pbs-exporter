package main

import "strconv"

// optInt and optBool format an optional setting as label value, "" when it
// is not set.
func optInt(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

func optBool(v *bool) string {
	if v == nil {
		return ""
	}
	return strconv.FormatBool(*v)
}

// keepOptions are the retention settings of prune jobs and datastores.
type keepOptions struct {
	KeepLast    *int64 `json:"keep-last"`
	KeepHourly  *int64 `json:"keep-hourly"`
	KeepDaily   *int64 `json:"keep-daily"`
	KeepWeekly  *int64 `json:"keep-weekly"`
	KeepMonthly *int64 `json:"keep-monthly"`
	KeepYearly  *int64 `json:"keep-yearly"`
}

// set returns the retention settings that are set, keyed by the keep label.
func (k keepOptions) set() map[string]int64 {
	out := map[string]int64{}
	for name, v := range map[string]*int64{
		"last": k.KeepLast, "hourly": k.KeepHourly, "daily": k.KeepDaily,
		"weekly": k.KeepWeekly, "monthly": k.KeepMonthly, "yearly": k.KeepYearly,
	} {
		if v != nil {
			out[name] = *v
		}
	}
	return out
}
