package storage

import (
	"sort"
	"strings"
	"time"
)

// lifecycleRule deletes a bucket's objects under a prefix once they're a
// number of days old, as a lifecycle rule with an age condition and a
// Delete action does.
type lifecycleRule struct {
	prefix  string
	ageDays int
}

// AddLifecycleRule gives a bucket a rule that deletes its objects whose
// names start with prefix once they're ageDays old, counted from their
// creation by the emulator's clock. Cloud Storage applies its rules on its
// own, some time after they're due; tests apply them with RunLifecycle.
func (e *Emulator) AddLifecycleRule(bucketName, prefix string, ageDays int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.buckets[bucketName]
	if b == nil {
		panic("storage emulator: no bucket " + bucketName)
	}
	b.rules = append(b.rules, lifecycleRule{prefix: prefix, ageDays: ageDays})
}

// RunLifecycle applies every bucket's lifecycle rules now, and returns the
// names of the objects they deleted, as <bucket>/<object>, in order.
func (e *Emulator) RunLifecycle() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	var gone []string
	for _, b := range e.buckets {
		for name, o := range b.objects {
			for _, ru := range b.rules {
				if strings.HasPrefix(name, ru.prefix) && now.Sub(o.created) >= time.Duration(ru.ageDays)*24*time.Hour {
					delete(b.objects, name)
					gone = append(gone, b.name+"/"+name)
					break
				}
			}
		}
	}
	sort.Strings(gone)
	return gone
}

// Attrs returns an object's size, generation and creation time, for tests
// to inspect.
func (e *Emulator) Attrs(bucketName, name string) (size, generation int64, created time.Time, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.buckets[bucketName]
	if b == nil || b.objects[name] == nil {
		return 0, 0, time.Time{}, false
	}
	o := b.objects[name]
	return int64(len(o.data)), o.generation, o.created, true
}
