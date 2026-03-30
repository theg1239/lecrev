package artifact

import "fmt"

func BuildLogsKey(buildJobID string) string {
	return fmt.Sprintf("builds/%s/logs.txt", buildJobID)
}

func ExecutionLogsKey(jobID, attemptID string) string {
	return fmt.Sprintf("executions/%s/%s/logs.txt", jobID, attemptID)
}

func ExecutionOutputKey(jobID, attemptID string) string {
	return fmt.Sprintf("executions/%s/%s/output.json", jobID, attemptID)
}
