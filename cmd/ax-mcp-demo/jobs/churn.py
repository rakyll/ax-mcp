# Train a churn model on customer features: logistic regression with
# mini-batch SGD, written out longhand because the sandbox has only the
# standard library. The customers are synthetic (seeded).
import csv, math, os, random
import axmcp

N, EPOCHS, LR, L2 = 200_000, 20, 0.02, 1e-4
FEATURES = ["tenure_months", "orders_90d", "avg_order_usd", "support_tickets", "days_since_order", "discount_share", "app_user", "apac"]
TRUE = [-0.6, -0.9, -0.2, 0.7, 1.2, 0.4, -0.1, 0.5]  # the hidden weights churn is drawn from

rng = random.Random(11)
axmcp.status(f"building features for {N:,} customers")
raw, y = [], []
for _ in range(N):
    x = [rng.expovariate(1 / 18), rng.expovariate(1 / 4), math.exp(rng.gauss(4, 0.6)), rng.expovariate(1 / 0.8),
         rng.expovariate(1 / 30), rng.random() * 0.6, float(rng.random() < 0.4), float(rng.random() < 0.2)]
    raw.append(x)
mean = [sum(col) / N for col in zip(*raw)]
std = [math.sqrt(sum((v - m) ** 2 for v in col) / N) for col, m in zip(zip(*raw), mean)]
X = [[(v - m) / s for v, m, s in zip(x, mean, std)] for x in raw]
del raw
for x in X:
    z = -1.3 + sum(w * v for w, v in zip(TRUE, x)) + rng.gauss(0, 0.8)
    y.append(1 if rng.random() < 1 / (1 + math.exp(-z)) else 0)

split = int(N * 0.8)
train, val = list(range(split)), list(range(split, N))


def predict(w, b, x):
    z = b + sum(wi * xi for wi, xi in zip(w, x))
    return 1 / (1 + math.exp(-max(-30, min(30, z))))


def auc(scores, labels):
    ranked = sorted(zip(scores, labels))
    pos = sum(labels)
    rank_sum = sum(r for r, (_, l) in enumerate(ranked, 1) if l)
    return (rank_sum - pos * (pos + 1) / 2) / (pos * (len(labels) - pos))


w, b = [0.0] * len(FEATURES), 0.0
for epoch in range(1, EPOCHS + 1):
    rng.shuffle(train)
    loss = 0.0
    for i in train:
        p = predict(w, b, X[i])
        g = p - y[i]
        loss -= math.log(p if y[i] else 1 - p)
        b -= LR * g
        w = [wi - LR * (g * xi + L2 * wi) for wi, xi in zip(w, X[i])]
    val_auc = auc([predict(w, b, X[i]) for i in val], [y[i] for i in val])
    axmcp.status(f"epoch {epoch}/{EPOCHS} · loss {loss / len(train):.4f} · val AUC {val_auc:.4f}")

scores = [predict(w, b, x) for x in X]
tp = sum(1 for i in val if scores[i] > 0.5 and y[i])
fp = sum(1 for i in val if scores[i] > 0.5 and not y[i])
fn = sum(1 for i in val if scores[i] <= 0.5 and y[i])
drivers = sorted(zip(w, FEATURES), key=lambda t: -abs(t[0]))[:3]
path = os.path.join(os.environ.get("AX_WORKSPACE") or os.getcwd(), "churn_scores.csv")
with open(path, "w", newline="") as f:
    csv.writer(f).writerows([("customer", "p_churn")] + [(i, f"{s:.4f}") for i, s in enumerate(scores)])
axmcp.result(
    f"val AUC {val_auc:.3f} (logistic regression, {len(train):,} train / {len(val):,} val, {EPOCHS} epochs)\n"
    f"top churn drivers: " + ", ".join(f"{name} {wt:+.2f}" for wt, name in drivers) + "\n"
    f"at p > 0.5: precision {tp / (tp + fp):.2f}, recall {tp / (tp + fn):.2f}\n"
    f"{sum(s > 0.7 for s in scores):,} of {N:,} customers at high risk (p > 0.7), scores in churn_scores.csv"
)
