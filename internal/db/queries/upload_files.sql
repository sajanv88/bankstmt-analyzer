-- name: CreateUploadFile :one
INSERT INTO upload_files (id, upload_id, position, filename, content_type, size_bytes, storage_key)
VALUES (
    sqlc.arg(id),
    sqlc.arg(upload_id),
    sqlc.arg(position),
    sqlc.arg(filename),
    sqlc.arg(content_type),
    sqlc.arg(size_bytes),
    sqlc.arg(storage_key)
)
RETURNING *;

-- name: ListUploadFiles :many
-- Ordered by position so the statements are always presented to the model
-- in the order the client submitted them.
SELECT * FROM upload_files
WHERE upload_id = sqlc.arg(upload_id)
ORDER BY position;
