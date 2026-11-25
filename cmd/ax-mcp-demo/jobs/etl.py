# Load September's payments export so revenue can be reconciled against
# orders. The export (synthetic, seeded) has rows a payment processor filled
# with "N/A"; the loader quarantines them instead of guessing.
import csv, random
import axmcp

BAD = {318_204, 402_117}


def write_export(path, rows=500_000):
    rng = random.Random(9)
    with open(path, "w", newline="") as f:
        out = csv.writer(f)
        out.writerow(["payment_id", "order_id", "processor", "amount_usd", "settled_at"])
        for i in range(1, rows + 1):
            amount = "N/A" if i in BAD else f"{rng.lognormvariate(4.1, 0.8):.2f}"
            out.writerow([f"pay_{i:07d}", rng.randrange(600_000), rng.choice(["stripe", "adyen", "paypal"]), amount, f"2026-09-{rng.randint(1, 30):02d}"])


axmcp.status("fetching payments_2026-09.csv")
write_export("payments_2026-09.csv")
total, loaded, rejected = {}, 0, []
for n, row in enumerate(csv.DictReader(open("payments_2026-09.csv")), 1):
    try:
        amount = float(row["amount_usd"])
    except ValueError:
        rejected.append((n + 1, row))  # +1 for the header line
        continue
    total[row["processor"]] = total.get(row["processor"], 0) + amount
    loaded += 1
    if n % 25_000 == 0:
        axmcp.status(f"parsing payments_2026-09.csv: {n:,} rows")

with open("payments_rejected.csv", "w", newline="") as f:
    out = csv.DictWriter(f, ["line", *rejected[0][1]] if rejected else ["line"])
    out.writeheader()
    out.writerows({"line": line, **row} for line, row in rejected)
axmcp.result(
    f"loaded {loaded:,} payments, ${sum(total.values()) / 1e6:.2f}M settled in September\n"
    f"by processor: " + ", ".join(f"{p} ${v / 1e6:.2f}M" for p, v in sorted(total.items())) + "\n"
    f"quarantined {len(rejected)} rows with amount_usd='N/A' (lines "
    + ", ".join(f"{line:,}" for line, _ in rejected) + ") in payments_rejected.csv"
)
