// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package taskset provides a generic work distribution mechanism for
// coordinating parallel workers. TaskSet hands out integer identifiers
// (TaskIDs) that workers can claim and process. The TaskIDs themselves have no
// inherent meaning - it's up to the caller to map each TaskID to actual work
// (e.g., file indices, key ranges, batch numbers, etc.).
//
// Example usage:
//
//	tasks := taskset.MakeTaskSet(100)  // Create 100 abstract work items (0-99)
//
//	// Worker goroutine
//	for taskID := tasks.ClaimFirst(); !taskID.IsDone(); taskID = tasks.ClaimNext(taskID) {
//	    // Map taskID to actual work
//	    processFile(files[taskID])
//	    // or: processKeyRange(splits[taskID], splits[taskID+1])
//	    // or: processBatch(taskID*batchSize, (taskID+1)*batchSize)
//	}
package taskset

import "github.com/cockroachdb/cockroach/pkg/util/syncutil"

// TaskID is an abstract integer identifier for a unit of work. The TaskID
// itself has no inherent meaning - callers decide what each TaskID represents
// (e.g., which file to process, which key range to handle, etc.).
type TaskID int64

// taskIDDone is an internal sentinel value indicating no more tasks are available.
// Use TaskID.IsDone() to check if a task is done.
const taskIDDone = TaskID(-1)

func (t TaskID) IsDone() bool {
	return t == taskIDDone
}

// MakeTaskSet creates a new TaskSet with taskCount work items numbered 0
// through taskCount-1. These are abstract identifiers that have no inherent
// meaning - the caller decides what each TaskID represents.
//
// For example, if you have 100 files to process, create MakeTaskSet(100) and
// map TaskID N to files[N]. Or for key range processing, map TaskID N to the
// range between splits[N-1] and splits[N].
func MakeTaskSet(taskCount int64) TaskSet {
	// TODO(jeffswenson): Should this be initialized with a set of initial
	// workers? Right now it doesn't start with a perfect span split, so if tasks
	// sizes are evenly distributed, we might split spans more than necessary.
	return TaskSet{
		unassigned: []taskSpan{{start: 0, end: TaskID(taskCount)}},
	}
}

// TaskSet is a generic work distribution coordinator that manages a collection
// of abstract task identifiers (TaskIDs) that can be claimed by workers.
//
// TaskSet implements a work-stealing algorithm optimized for task locality:
// - When a worker completes task N, it tries to claim task N+1 (sequential locality)
// - If task N+1 is unavailable, it splits the largest remaining span of unclaimed tasks
// - This balances load across workers while maintaining locality within each worker
//
// The TaskIDs themselves are just integers (0 through taskCount-1) with no
// inherent meaning. Callers map these identifiers to actual work units such as:
// - File indices (TaskID 5 → process files[5])
// - Key ranges (TaskID 5 → process range [splits[4], splits[5]))
// - Batch numbers (TaskID 5 → process rows [5000, 6000))
//
// TaskSet is safe for concurrent use by multiple goroutines.
type TaskSet struct {
	mu         syncutil.Mutex
	unassigned []taskSpan
}

// ClaimFirst should be called when a worker claims its first task. It returns
// an abstract TaskID to process. The caller decides what this TaskID represents
// (e.g., which file to process, which key range to handle). Returns a TaskID
// where .IsDone() is true if no tasks are available.
//
// ClaimFirst is distinct from ClaimNext because ClaimFirst will always split
// the largest span in the unassigned set, whereas ClaimNext will assign from
// the same span until it is exhausted.
func (t *TaskSet) ClaimFirst() TaskID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.claimFirstLocked()
}

// claimFirstLocked is the internal implementation of ClaimFirst.
// The caller must hold t.mu.
func (t *TaskSet) claimFirstLocked() TaskID {
	if len(t.unassigned) == 0 {
		return taskIDDone
	}

	// Find the largest span
	largest := 0
	for i := range t.unassigned {
		if t.unassigned[largest].size() < t.unassigned[i].size() {
			largest = i
		}
	}

	largestSpan := t.unassigned[largest]
	if largestSpan.size() == 0 {
		return taskIDDone
	}
	if largestSpan.size() == 1 {
		t.lockedRemoveSpan(largest)
		return largestSpan.start
	}

	left, right := largestSpan.split()
	t.unassigned[largest] = left

	task := right.start
	right.start += 1
	if right.size() != 0 {
		t.insertSpan(right, largest+1)
	}

	return task
}

// ClaimNext should be called when a worker has completed its current task. It
// returns the next abstract TaskID to process. The caller decides what this
// TaskID represents. Returns a TaskID where .IsDone() is true if no tasks are
// available.
//
// ClaimNext optimizes for locality by attempting to claim lastTask+1 first. If
// that task is unavailable, it falls back to ClaimFirst behavior (splitting the
// largest remaining span).
func (t *TaskSet) ClaimNext(lastTask TaskID) TaskID {
	t.mu.Lock()
	defer t.mu.Unlock()

	next := lastTask + 1

	for i, span := range t.unassigned {
		if span.start != next {
			continue
		}

		span.start += 1

		if span.size() == 0 {
			t.lockedRemoveSpan(i)
			return next
		}

		t.unassigned[i] = span
		return next
	}

	// If we didn't find the next task in the unassigned set, then we've
	// exhausted the span and need to claim from a different span.
	// We already hold the lock, so call the internal implementation.
	return t.claimFirstLocked()
}

func (t *TaskSet) insertSpan(span taskSpan, index int) {
	t.unassigned = append(t.unassigned, taskSpan{})
	copy(t.unassigned[index+1:], t.unassigned[index:])
	t.unassigned[index] = span
}

func (t *TaskSet) lockedRemoveSpan(index int) {
	t.unassigned = append(t.unassigned[:index], t.unassigned[index+1:]...)
}
