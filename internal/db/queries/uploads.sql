-- name: CreateUpload :one
INSERT INTO uploads (id, status, file_count)
VALUES (sqlc.arg(id), sqlc.arg(status), sqlc.arg(file_count))
RETURNING *;

-- name: GetUpload :one
SELECT * FROM uploads WHERE id = sqlc.arg(id);

-- name: SetUploadStatus :one
UPDATE uploads
SET status         = sqlc.arg(status),
    failure_reason = sqlc.narg(failure_reason),
    updated_at     = now()
WHERE id = sqlc.arg(id)
RETURNING *;
