# Forecast the next four weeks of revenue. A few huge B2B orders would skew
# the model, and whether they belong in it is a business call, so the job
# asks the user through MCP elicitation before fitting. The orders are
# synthetic (seeded).
import array, math, random, statistics
import axmcp

DAYS, HORIZON = 273, 28
rng = random.Random(3)

axmcp.status("loading 2026 orders")
days, amounts = array.array("H"), array.array("d")  # one entry per order
for day in range(DAYS):
    weekly = [1.0, 1.02, 1.05, 1.03, 1.1, 1.25, 1.18][day % 7]
    for _ in range(int(rng.gauss(4600, 180) * weekly * (1 + day / 9000))):
        days.append(day)
        amounts.append(math.exp(rng.gauss(4.1, 0.8)))
for _ in range(37):  # bulk orders from a handful of partner accounts
    days.append(rng.randrange(DAYS))
    amounts.append(rng.uniform(19_000, 212_500))

axmcp.status(f"checking {len(amounts):,} order amounts for outliers")
logs = sorted(math.log(a) for a in amounts)
q1, q3 = logs[len(logs) // 4], logs[3 * len(logs) // 4]
cap = math.exp(q3 + 4 * (q3 - q1))
outliers = [a for a in amounts if a > cap]
total = sum(amounts)

answer = axmcp.ask(
    f"{len(outliers)} orders of ${min(outliers):,.0f} to ${max(outliers):,.0f} sit far above the usual "
    f"${math.exp(logs[len(logs) // 2]):,.0f} median and make up {sum(outliers) / total:.1%} of revenue. "
    f"How should the forecast treat them?",
    {
        "type": "object",
        "properties": {
            "action": {"type": "string", "title": "Treatment", "enum": ["drop", "clip", "keep"], "default": "clip"},
            "reason": {"type": "string", "title": "Reason (for the report)"},
        },
        "required": ["action"],
    },
)
content = answer.get("content") or {}
action = content.get("action", "keep") if answer.get("action") == "accept" else "keep"

daily = [0.0] * DAYS
for day, a in zip(days, amounts):
    if a > cap and action == "drop":
        continue
    daily[day] += min(a, cap) if action == "clip" else a


def holt_winters(series, alpha, beta, gamma, season=7):
    level, trend = sum(series[:season]) / season, 0.0
    seasonal = [v - level for v in series[:season]]
    fitted = []
    for t, v in enumerate(series):
        s = seasonal[t % season]
        fitted.append(level + trend + s)
        last = level
        level = alpha * (v - s) + (1 - alpha) * (level + trend)
        trend = beta * (level - last) + (1 - beta) * trend
        seasonal[t % season] = gamma * (v - level) + (1 - gamma) * s
    return fitted, lambda h: level + h * trend + seasonal[(len(series) + h - 1) % season]


grid = [(a / 20, b / 20, g / 20) for a in range(1, 20) for b in range(0, 10) for g in range(1, 20)]
folds = [DAYS - HORIZON * k for k in (3, 2, 1)]  # rolling-origin cross-validation
best = None
for n, (a, b, g) in enumerate(grid, 1):
    if n % 200 == 0:
        axmcp.status(f"fitting Holt-Winters: {n}/{len(grid)} parameter sets")
    errors = []
    for end in folds:
        _, fc = holt_winters(daily[:end], a, b, g)
        errors += [abs(fc(h) - daily[end + h - 1]) / daily[end + h - 1] for h in range(1, HORIZON + 1)]
    mape = statistics.fmean(errors)
    if best is None or mape < best[0]:
        best = (mape, a, b, g)

mape, a, b, g = best
fitted, fc = holt_winters(daily, a, b, g)
resid = statistics.stdev(v - f for v, f in zip(daily[7:], fitted[7:]))
weeks = [sum(fc(h) for h in range(w * 7 + 1, w * 7 + 8)) for w in range(4)]
treated = {"drop": "dropped", "clip": f"clipped to the ${cap:,.0f} 4×IQR fence", "keep": "kept"}[action]
axmcp.result(
    f"outliers: {len(outliers)} bulk orders {treated}; reason: {content.get('reason') or 'none given'}\n"
    f"model: Holt-Winters (additive, weekly season), α={a:.2f} β={b:.2f} γ={g:.2f}, CV MAPE {mape:.1%}\n"
    f"next 4 weeks: " + ", ".join(f"${v / 1e6:.2f}M" for v in weeks) + f" (80% PI ±${1.28 * resid * math.sqrt(7) / 1e3:,.0f}k/week)"
)
