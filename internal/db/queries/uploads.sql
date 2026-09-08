-- name: CreateUpload :one
INSERT INTO uploads (id, status, file_count)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUpload :one
SELECT * FROM uploads WHERE id = $1;
