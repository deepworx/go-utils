// Package aggregate is the domain-facing side of event sourcing. Domain code
// imports only this package; it depends on nothing but the stdlib and uuid.
//
// An Aggregate embeds Base by value and implements StreamType and Apply on its
// pointer type. Commands validate and call Raise, which runs the same Apply
// that rebuilds state from history:
//
//	type TaskID uuid.UUID
//
//	type Task struct {
//		aggregate.Base[TaskID]
//		done bool
//	}
//
//	func (*Task) StreamType() string { return "task" }
//
//	func (t *Task) Apply(e aggregate.Event) {
//		switch e.(type) {
//		case TaskCompleted:
//			t.done = true
//		}
//	}
//
//	func (t *Task) Complete() error {
//		if t.done {
//			return ErrAlreadyDone
//		}
//		aggregate.Raise(t, TaskCompleted{})
//		return nil
//	}
//
// Root is sealed: a type that does not embed Base cannot satisfy it, even if
// it declares StreamType, Version and Apply.
//
// # Keeping domain packages pure
//
// Services can let golangci-lint (v2) enforce that their domain packages
// import only this package, the stdlib and uuid:
//
//	linters:
//	  enable:
//	    - depguard
//	  settings:
//	    depguard:
//	      rules:
//	        domain:
//	          list-mode: strict
//	          files:
//	            - "**/internal/domain/**"
//	          allow:
//	            - $gostd
//	            - github.com/google/uuid
//	            - github.com/deepworx/go-utils/pkg/eventsourcing/aggregate
//
// Adjust the files glob to where the service keeps its domain code.
package aggregate
