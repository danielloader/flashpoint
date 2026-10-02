package watch

import "time"

// Debounce gathers what arrives on in and sends it as one batch once in has
// been quiet for d: a trailing debounce, so a burst of saves (a formatter, a
// git checkout) is one rebuild however long it lasts. Each name appears once
// per batch, in first-seen order. out closes after in does.
func Debounce(in <-chan string, d time.Duration) <-chan []string {
	out := make(chan []string, 1)
	go func() {
		defer close(out)
		var (
			batch []string
			seen  = map[string]bool{}
			timer = time.NewTimer(d)
		)
		timer.Stop()
		for {
			select {
			case name, ok := <-in:
				if !ok {
					timer.Stop()
					return
				}
				if !seen[name] {
					seen[name] = true
					batch = append(batch, name)
				}
				timer.Reset(d)
			case <-timer.C:
				if len(batch) == 0 {
					continue
				}
				// The consumer may be mid-build: fold into a waiting batch
				// rather than block the watcher.
				select {
				case prev := <-out:
					batch = merge(prev, batch)
				default:
				}
				out <- batch
				batch, seen = nil, map[string]bool{}
			}
		}
	}()
	return out
}

func merge(a, b []string) []string {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			a = append(a, s)
		}
	}
	return a
}
