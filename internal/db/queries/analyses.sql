-- name: GetAnalysisByUpload :one
SELECT * FROM analyses WHERE upload_id = sqlc.arg(upload_id);

-- name: GetAnalysisIDByUpload :one
SELECT id FROM analyses WHERE upload_id = sqlc.arg(upload_id);
