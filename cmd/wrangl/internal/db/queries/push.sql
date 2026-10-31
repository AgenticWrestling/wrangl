-- name: UpsertPushSubscription :exec
INSERT INTO push_subscription (endpoint, p256dh, auth, user_agent)
VALUES (?, ?, ?, ?)
ON CONFLICT (endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth,
                                     user_agent = excluded.user_agent, failures = 0;
