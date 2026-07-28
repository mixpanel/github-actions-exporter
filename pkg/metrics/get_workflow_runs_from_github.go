package metrics

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/spendesk/github-actions-exporter/pkg/config"

	"github.com/google/go-github/v45/github"
)

// getFieldValue return value from run element which corresponds to field
func getFieldValue(repo string, run github.WorkflowRun, field string) string {
	switch field {
	case "repo":
		return repo
	case "id":
		return strconv.FormatInt(*run.ID, 10)
	case "node_id":
		return *run.NodeID
	case "head_branch":
		return *run.HeadBranch
	case "head_sha":
		return *run.HeadSHA
	case "run_number":
		return strconv.Itoa(*run.RunNumber)
	case "workflow_id":
		return strconv.FormatInt(*run.WorkflowID, 10)
	case "workflow":
		r, exist := workflows[repo]
		if !exist {
			log.Printf("Couldn't fetch repo '%s' from workflow cache.", repo)
			return "unknown"
		}
		w, exist := r[*run.WorkflowID]
		if !exist {
			log.Printf("Couldn't fetch repo '%s', workflow '%d' from workflow cache.", repo, *run.WorkflowID)
			return "unknown"
		}
		return *w.Name
	case "event":
		return *run.Event
	case "status":
		return *run.Status
	}
	log.Printf("Tried to fetch invalid field '%s'", field)
	return ""
}

func getRelevantFields(repo string, run *github.WorkflowRun) []string {
	relevantFields := strings.Split(config.WorkflowFields, ",")
	result := make([]string, len(relevantFields))
	for i, field := range relevantFields {
		result[i] = getFieldValue(repo, *run, field)
	}
	return result
}

func getRecentWorkflowRuns(owner string, repo string) []*github.WorkflowRun {
	window_start := time.Now().Add(time.Duration(-12) * time.Hour).Format(time.RFC3339)
	opt := &github.ListWorkflowRunsOptions{
		ListOptions: github.ListOptions{PerPage: 200},
		Created:     ">=" + window_start,
	}

	var runs []*github.WorkflowRun
	for {
		resp, rr, err := client.Actions.ListRepositoryWorkflowRuns(context.Background(), owner, repo, opt)
		if rl_err, ok := err.(*github.RateLimitError); ok {
			log.Printf("ListRepositoryWorkflowRuns ratelimited. Pausing until %s", rl_err.Rate.Reset.Time.String())
			time.Sleep(time.Until(rl_err.Rate.Reset.Time))
			continue
		} else if err != nil {
			log.Printf("ListRepositoryWorkflowRuns error for repo %s/%s: %s", owner, repo, err.Error())
			return runs
		}

		runs = append(runs, resp.WorkflowRuns...)
		if rr.NextPage == 0 {
			break
		}
		opt.Page = rr.NextPage
	}

	return runs
}

func getRunUsage(owner string, repo string, runId int64) *github.WorkflowRunUsage {
	for {
		resp, _, err := client.Actions.GetWorkflowRunUsageByID(context.Background(), owner, repo, runId)
		if rl_err, ok := err.(*github.RateLimitError); ok {
			log.Printf("GetWorkflowRunUsageByID ratelimited. Pausing until %s", rl_err.Rate.Reset.Time.String())
			time.Sleep(time.Until(rl_err.Rate.Reset.Time))
			continue
		} else if err != nil {
			log.Printf("GetWorkflowRunUsageByID error for repo %s/%s and runId %d: %s", owner, repo, runId, err.Error())
			return nil
		}
		return resp
	}
}

// getWorkflowJobs - list all jobs (with their steps) for a workflow run
func getWorkflowJobs(owner string, repo string, runId int64) []*github.WorkflowJob {
	opt := &github.ListWorkflowJobsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	var jobs []*github.WorkflowJob
	for {
		resp, rr, err := client.Actions.ListWorkflowJobs(context.Background(), owner, repo, runId, opt)
		if rl_err, ok := err.(*github.RateLimitError); ok {
			log.Printf("ListWorkflowJobs ratelimited. Pausing until %s", rl_err.Rate.Reset.Time.String())
			time.Sleep(time.Until(rl_err.Rate.Reset.Time))
			continue
		} else if err != nil {
			log.Printf("ListWorkflowJobs error for repo %s/%s and runId %d: %s", owner, repo, runId, err.Error())
			return jobs
		}

		jobs = append(jobs, resp.Jobs...)
		if rr.NextPage == 0 {
			break
		}
		opt.Page = rr.NextPage
	}

	return jobs
}

// stepMetricsWorkflowAllowed reports whether step metrics should be emitted for
// the given workflow name, honoring the optional allowlist.
func stepMetricsWorkflowAllowed(workflow string) bool {
	allow := strings.TrimSpace(config.Metrics.WorkflowStepMetricsWorkflows)
	if allow == "" {
		return true
	}
	for _, w := range strings.Split(allow, ",") {
		if strings.TrimSpace(w) == workflow {
			return true
		}
	}
	return false
}

// emitWorkflowStepMetrics fetches the jobs of a completed run and sets a
// per-step duration gauge for every step with both start and completion times.
func emitWorkflowStepMetrics(owner string, repo string, run *github.WorkflowRun) {
	// The workflow cache is keyed by the full "owner/name" (see periodicGithubFetcher),
	// and the run-gauge path (getRelevantFields) looks it up that way. Use the same key
	// here, otherwise getFieldValue misses the cache on every completed run: it logs
	// "Couldn't fetch repo '<name>' from workflow cache." and falls back to "unknown",
	// which also breaks the WorkflowStepMetricsWorkflows allowlist (nothing matches
	// "unknown", so scoping the feature would emit zero step metrics).
	fullRepo := owner + "/" + repo
	workflow := getFieldValue(fullRepo, *run, "workflow")
	if !stepMetricsWorkflowAllowed(workflow) {
		return
	}
	runId := strconv.FormatInt(run.GetID(), 10)
	for _, job := range getWorkflowJobs(owner, repo, run.GetID()) {
		for _, step := range job.Steps {
			if step.StartedAt == nil || step.CompletedAt == nil {
				continue
			}
			seconds := step.CompletedAt.Time.Sub(step.StartedAt.Time).Seconds()
			if seconds < 0 {
				continue
			}
			// Use fullRepo for the repo label too, to match github_workflow_run_* gauges.
			workflowStepDurationGauge.WithLabelValues(
				fullRepo, workflow, job.GetName(), step.GetName(), step.GetConclusion(), runId,
			).Set(seconds)
		}
	}
}

// getWorkflowRunsFromGithub - return informations and status about a workflow
func getWorkflowRunsFromGithub() {
	for {
		for _, repo := range repositories {
			r := strings.Split(repo, "/")
			runs := getRecentWorkflowRuns(r[0], r[1])

			for _, run := range runs {
				var s float64 = 0
				if run.GetConclusion() == "success" {
					s = 1
				} else if run.GetConclusion() == "skipped" {
					s = 2
				} else if run.GetConclusion() == "in_progress" {
					s = 3
				} else if run.GetConclusion() == "queued" {
					s = 4
				}

				fields := getRelevantFields(repo, run)

				workflowRunStatusGauge.WithLabelValues(fields...).Set(s)

				var run_usage *github.WorkflowRunUsage = nil
				if config.Metrics.FetchWorkflowRunUsage {
					run_usage = getRunUsage(r[0], r[1], *run.ID)
				}
				if run_usage == nil { // Fallback for Github Enterprise
					created := run.CreatedAt.Time.Unix()
					updated := run.UpdatedAt.Time.Unix()
					elapsed := updated - created
					workflowRunDurationGauge.WithLabelValues(fields...).Set(float64(elapsed * 1000))
				} else {
					workflowRunDurationGauge.WithLabelValues(fields...).Set(float64(run_usage.GetRunDurationMS()))
				}

				if config.Metrics.FetchWorkflowStepMetrics && run.GetStatus() == "completed" {
					emitWorkflowStepMetrics(r[0], r[1], run)
				}
			}
		}

		time.Sleep(time.Duration(config.Github.Refresh) * time.Second)
		workflowRunStatusGauge.Reset()
		workflowRunDurationGauge.Reset()
		workflowStepDurationGauge.Reset()
	}
}
