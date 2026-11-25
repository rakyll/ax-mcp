# Grid-search hyperparameters for the retention model: 2,160 trials, each one a
# 5-fold cross-validated training run. In pure Python that takes hours, which
# is the point: the agent sees the ETA in statusMessage and cancels the task.
# On SIGTERM the job reports the best trial so far as its result.
import itertools, math, random, signal, sys, time
import axmcp

rng = random.Random(5)
X = [[rng.gauss(0, 1) for _ in range(8)] for _ in range(20_000)]
TRUE = [-0.6, -0.9, -0.2, 0.7, 1.2, 0.4, -0.1, 0.5]
y = [1 if rng.random() < 1 / (1 + math.exp(-sum(w * v for w, v in zip(TRUE, x)))) else 0 for x in X]
folds = [range(k, len(X), 5) for k in range(5)]

grid = list(itertools.product([0.003, 0.01, 0.03, 0.1, 0.3, 1.0], [0, 1e-5, 1e-4, 1e-3, 1e-2], [5, 10, 20, 40], [16, 32, 64, 128, 256, 512], [0.0, 0.5, 0.9]))
rng.shuffle(grid)  # sample the grid in random order, so early trials are representative
work = sum(epochs for _, _, epochs, _, _ in grid)


def fit_and_score(val, lr, l2, epochs, batch, momentum):
    train = [i for f in folds if f is not val for i in f]
    w, v = [0.0] * 8, [0.0] * 8
    for _ in range(epochs):
        for start in range(0, len(train), batch):
            g = [0.0] * 8
            for i in train[start:start + batch]:
                p = 1 / (1 + math.exp(-max(-30, min(30, sum(a * b for a, b in zip(w, X[i]))))))
                g = [gj + (p - y[i]) * xj for gj, xj in zip(g, X[i])]
            v = [momentum * vj + gj / batch + l2 * wj for vj, gj, wj in zip(v, g, w)]
            w = [wj - lr * vj for wj, vj in zip(w, v)]
    scores = sorted((max(-30, min(30, sum(a * b for a, b in zip(w, X[i])))), y[i]) for i in val)
    pos = sum(l for _, l in scores)
    auc = (sum(r for r, (_, l) in enumerate(scores, 1) if l) - pos * (pos + 1) / 2) / (pos * (len(scores) - pos))
    loss = sum(math.log1p(math.exp(-z if l else z)) for z, l in scores) / len(scores)
    return auc, loss


axmcp.status(f"trial 1/{len(grid):,} · no results yet")
best, best_params, done, t0, n = math.inf, None, 0, time.monotonic(), 0


def stop(*_):
    if best_params is None:
        axmcp.result("stopped before the first trial finished")
        sys.exit(0)
    lr, l2, epochs, batch, momentum = best_params
    axmcp.result(
        f"stopped after {n - 1} of {len(grid):,} trials; the full search would take hours\n"
        f"best so far: lr={lr} l2={l2} epochs={epochs} batch={batch} momentum={momentum}, CV log loss {best:.4f}\n"
        f"suggestion: random-search 50 trials, or tune lr and l2 only"
    )
    sys.exit(0)


signal.signal(signal.SIGTERM, stop)
for n, params in enumerate(grid, 1):
    lr, l2, epochs, batch, momentum = params
    aucs, losses = zip(*(fit_and_score(f, *params) for f in folds))
    auc, loss = sum(aucs) / len(folds), sum(losses) / len(folds)
    if loss < best:
        best, best_params = loss, params
    done += epochs
    eta = (time.monotonic() - t0) / done * (work - done)
    print(f"trial {n}: lr={lr} l2={l2} epochs={epochs} batch={batch} momentum={momentum} → CV AUC {auc:.4f}, log loss {loss:.4f}")
    axmcp.status(f"trial {n}/{len(grid):,} · best log loss {best:.4f} · ETA {eta / 3600:.1f}h")
