# Profile 2026's orders table and look for what changed in Q3: the first
# thing a data science agent does with a revenue question. The orders are
# synthetic (seeded) and loaded into SQLite (orders.db in the workspace).
import datetime, math, random, sqlite3
import axmcp

N = 1_500_000
REGIONS = {"NA": 0.42, "EMEA": 0.30, "APAC": 0.20, "LATAM": 0.08}
CHANNELS = {"web": 0.45, "app": 0.35, "partner": 0.12, "sales": 0.08}
START, DAYS = datetime.date(2026, 1, 1), 273  # through Sep 30
BREAK = datetime.date(2026, 8, 14)  # app release 7.2: checkout breaks on some APAC devices

rng = random.Random(7)
region_names, region_w = list(REGIONS), list(REGIONS.values())
channel_names, channel_w = list(CHANNELS), list(CHANNELS.values())


def orders():
    i = 0
    while i < N:
        day = START + datetime.timedelta(days=rng.randrange(DAYS))
        region = rng.choices(region_names, region_w)[0]
        channel = rng.choices(channel_names, channel_w)[0]
        if day >= BREAK and region == "APAC" and channel == "app" and rng.random() < 0.6:
            continue  # the order never happened
        i += 1
        if i % 100_000 == 0:
            axmcp.status(f"loading orders: {i:,}/{N:,} rows")
        amount = round(math.exp(rng.gauss(4.1, 0.8)), 2)
        discount = None if rng.random() < 0.031 else round(rng.choice([0, 0, 0, 0.05, 0.1, 0.2]) * amount, 2)
        yield (i, day.isoformat(), region, channel, rng.randrange(90_000), amount, discount, int(rng.random() < 0.024))


db = sqlite3.connect("orders.db")
db.execute("drop table if exists orders")
db.execute("create table orders (id integer primary key, day text, region text, channel text, customer integer, amount real, discount real, refunded integer)")
db.executemany("insert into orders values (?,?,?,?,?,?,?,?)", orders())

cols = ["day", "region", "channel", "customer", "amount", "discount", "refunded"]
nulls = {}
for n, c in enumerate(cols, 1):
    axmcp.status(f"profiling column {n}/{len(cols)}: {c}")
    nulls[c] = db.execute(f"select count(*) from orders where {c} is null").fetchone()[0]
    db.execute(f"select count(distinct {c}), min({c}), max({c}) from orders").fetchone()

axmcp.status("revenue by quarter")
q = dict(db.execute("""select (cast(substr(day, 6, 2) as int) + 2) / 3, sum(amount - coalesce(discount, 0))
                       from orders where refunded = 0 group by 1"""))

axmcp.status("scanning region × channel segments for a change point")
best = None
for region in REGIONS:
    for channel in CHANNELS:
        daily = dict(db.execute("select day, count(*) from orders where region = ? and channel = ? group by day", (region, channel)))
        days = sorted(daily)
        for split in range(len(days) - 120, len(days) - 14):
            before = sum(daily[d] for d in days[split - 28:split]) / 28
            after = sum(daily[d] for d in days[split:split + 14]) / 14
            change = after / before - 1
            if best is None or change < best[0]:
                best = (change, region, channel, days[split])

change, region, channel, day = best
nulls = ", ".join(f"{c} {nulls[c] / N:.1%}" for c in cols if nulls[c]) or "none"
axmcp.result(
    f"orders: {N:,} rows × {len(cols)} columns, {START} … {START + datetime.timedelta(days=DAYS - 1)}; nulls: {nulls}\n"
    f"net revenue: Q1 ${q[1] / 1e6:.2f}M, Q2 ${q[2] / 1e6:.2f}M, Q3 ${q[3] / 1e6:.2f}M ({q[3] / q[2] - 1:+.1%} vs Q2)\n"
    f"biggest shift: {region}/{channel} daily orders {change:+.0%} from {day} (change point)\n"
    f"next: check what shipped to the {region} {channel} around {day}"
)
