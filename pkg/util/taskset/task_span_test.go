// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package taskset

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskSetSingleWorker(t *testing.T) {
	// When a single worker claims tasks from a taskSet, it should claim all
	// tasks in sequential order now that we use FIFO claiming.
	tasks := MakeTaskSet(10, 1)
	var found []TaskID

	for next := tasks.ClaimFirst(); !next.IsDone(); next = tasks.ClaimNext(next) {
		found = append(found, next)
	}

	// With the new FIFO approach, tasks are claimed sequentially
	require.Equal(t, []TaskID{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, found)
}

func TestTaskSetParallel(t *testing.T) {
	taskCount := min(rand.Int63n(10000), 16)
	tasks := MakeTaskSet(taskCount, 16)
	workers := make([]TaskID, 16)
	var found []TaskID

	for i := range workers {
		workers[i] = tasks.ClaimFirst()
		found = append(found, workers[i])
	}

	for {
		// pick a random worker to claim the next task
		workerIndex := rand.Intn(len(workers))
		prevTask := workers[workerIndex]
		next := tasks.ClaimNext(prevTask)
		if next.IsDone() {
			break
		}
		workers[workerIndex] = next
		found = append(found, next)
	}

	// build a map of the found tasks to ensure they are unique
	taskMap := make(map[TaskID]struct{})
	for _, task := range found {
		taskMap[task] = struct{}{}
	}
	require.Len(t, taskMap, int(taskCount))
}

func TestMakeTaskSet(t *testing.T) {
	// Test with evenly divisible tasks - simulate 4 workers each calling ClaimFirst once
	tasks := MakeTaskSet(100, 4)
	var claimed []TaskID
	for i := 0; i < 4; i++ {
		task := tasks.ClaimFirst()
		require.False(t, task.IsDone())
		claimed = append(claimed, task)
	}
	// Each worker should get the first task from their region (round-robin)
	require.Equal(t, []TaskID{0, 25, 50, 75}, claimed)

	// Test with tasks that don't divide evenly - simulate 3 workers
	tasks = MakeTaskSet(100, 3)
	claimed = nil
	for i := 0; i < 3; i++ {
		task := tasks.ClaimFirst()
		require.False(t, task.IsDone())
		claimed = append(claimed, task)
	}
	// First span gets 34 tasks [0,34), second gets 33 [34,67), third gets 33 [67,100)
	require.Equal(t, []TaskID{0, 34, 67}, claimed)

	// Test with more workers than tasks - simulate 5 workers (only 5 tasks available)
	tasks = MakeTaskSet(5, 10)
	claimed = nil
	for i := 0; i < 5; i++ {
		task := tasks.ClaimFirst()
		require.False(t, task.IsDone())
		claimed = append(claimed, task)
	}
	require.Equal(t, []TaskID{0, 1, 2, 3, 4}, claimed)
	// 6th worker should get nothing
	require.True(t, tasks.ClaimFirst().IsDone())

	// Test edge cases
	tasks = MakeTaskSet(0, 4)
	require.True(t, tasks.ClaimFirst().IsDone())

	tasks = MakeTaskSet(10, 0)
	require.False(t, tasks.ClaimFirst().IsDone()) // Should default to 1 worker

	tasks = MakeTaskSet(10, -1)
	require.False(t, tasks.ClaimFirst().IsDone()) // Should default to 1 worker
}

func TestTaskSetLoadBalancing(t *testing.T) {
	// Simulate 4 workers processing 100 tasks
	tasks := MakeTaskSet(100, 4)

	type worker struct {
		id    int
		tasks []TaskID
	}
	workers := make([]worker, 4)

	// Each worker claims their first task
	for i := range workers {
		workers[i].id = i
		task := tasks.ClaimFirst()
		require.False(t, task.IsDone())
		workers[i].tasks = append(workers[i].tasks, task)
	}

	// Verify initial distribution is balanced across regions
	require.Equal(t, TaskID(0), workers[0].tasks[0])  // Region [0, 25)
	require.Equal(t, TaskID(25), workers[1].tasks[0]) // Region [25, 50)
	require.Equal(t, TaskID(50), workers[2].tasks[0]) // Region [50, 75)
	require.Equal(t, TaskID(75), workers[3].tasks[0]) // Region [75, 100)

	// Simulate concurrent-like processing: round-robin through workers
	// This prevents one worker from stealing all the work
	for {
		claimed := false
		for i := range workers {
			lastTask := workers[i].tasks[len(workers[i].tasks)-1]
			next := tasks.ClaimNext(lastTask)
			if !next.IsDone() {
				workers[i].tasks = append(workers[i].tasks, next)
				claimed = true
			}
		}
		if !claimed {
			break
		}
	}

	// Verify all tasks were claimed exactly once
	allTasks := make(map[TaskID]bool)
	for _, w := range workers {
		for _, task := range w.tasks {
			require.False(t, allTasks[task], "task %d claimed multiple times", task)
			allTasks[task] = true
		}
	}
	require.Len(t, allTasks, 100)

	// With round-robin processing, each worker should get approximately equal work
	for i, w := range workers {
		require.InDelta(t, 25, len(w.tasks), 2, "worker %d got %d tasks", i, len(w.tasks))
	}
}
