-- name: UpsertPushSubscription :exec
INSERT INTO push_subscription (endpoint, p256dh, auth, user_agent)
VALUES (?, ?, ?, ?)
ON CONFLICT (endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth,
                                     user_agent = excluded.user_agent, failures = 0;

-- name: ListPushSubscriptions :many
SELECT * FROM push_subscription ORDER BY id;

-- name: DeletePushSubscription :exec
DELETE FROM push_subscription WHERE endpoint = ?;

-- name: PushSucceeded :exec
UPDATE push_subscription SET failures = 0, last_success_at = ? WHERE id = ?;

-- name: PushFailed :exec
UPDATE push_subscription SET failures = failures + 1 WHERE id = ?;
