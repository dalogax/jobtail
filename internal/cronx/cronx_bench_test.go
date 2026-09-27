// Next is called far more often than "once per schedule": `jobtail tick`
// calls it for every enabled job once a minute, and the dashboard's Next
// column calls it for every job each time the jobs table is rebuilt. The two
// cases below exist to keep the gap between them closed — see locCache for
// what used to open it.
package cronx

import (
	"testing"
	"time"
)

func BenchmarkNextLocalTimezone(b *testing.B) {
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Next("*/5 * * * *", "local", now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNextIANATimezone(b *testing.B) {
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Next("*/5 * * * *", "Europe/Madrid", now); err != nil {
			b.Fatal(err)
		}
	}
}
