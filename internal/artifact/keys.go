package artifact

import "fmt"

func ExecutionLogsKey(jobID, attemptID string) string {
	return fmt.Sprintf("executions/%s/%s/logs.txt", jobID, attemptID)
}

func ExecutionOutputKey(jobID, attemptID string) string {
	return fmt.Sprintf("executions/%s/%s/output.json", jobID, attemptID)
}
