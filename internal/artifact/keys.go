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

func SiteManifestKey(digest string) string {
	return fmt.Sprintf("artifacts/%s/site-manifest.json", digest)
}

func SiteAssetsPrefix(digest string) string {
	return fmt.Sprintf("sites/%s/assets", digest)
}

func SiteAssetObjectKey(digest, assetPath string) string {
	if assetPath == "" || assetPath == "/" {
		return SiteAssetsPrefix(digest) + "/index.html"
	}
	if assetPath[0] == '/' {
		return SiteAssetsPrefix(digest) + assetPath
	}
	return SiteAssetsPrefix(digest) + "/" + assetPath
}
