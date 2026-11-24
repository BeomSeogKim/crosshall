"""Compare a store before and after its migrations for scripts/release-gate.sh.

Usage: release_gate_rows.py <before.sqlite3> <after.sqlite3> <migration.sql>...

Every table, column, row and index of the store before must survive, except
for what the migrations' own statements select:
- a row that an UPDATE's WHERE selects in the store before may change in the
  columns its SET names;
- a row that a DELETE's WHERE selects may be gone;
- a table that a migration INSERTs into may gain rows.
The WHERE clauses are evaluated on the store before. A statement this script
cannot read, such as WITH ... DELETE, allows nothing, so it fails closed.
Exit 0 when the store after holds no other change, 1 otherwise; the output
says what each migration changed.
"""

import re
import sqlite3
import sys

WRITE = re.compile(r'^(UPDATE|DELETE\s+FROM|INSERT\s+(?:OR\s+\w+\s+)?INTO)\s+"?(\w+)"?', re.I)


def statements(sql):
    pending = ""
    for line in sql.splitlines(keepends=True):
        pending += line
        if sqlite3.complete_statement(pending):
            yield pending.strip()
            pending = ""


def top_level(text):
    """The indexes of text outside parentheses and quotes."""
    depth, quote = 0, None
    for i, c in enumerate(text):
        if quote:
            quote = None if c == quote else quote
        elif c in "'\"":
            quote = c
        elif c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
        elif depth == 0:
            yield i


def keyword(text, word, start=0):
    """The index of the first word outside parentheses and quotes, or -1."""
    for i in top_level(text):
        if i >= start and re.match(rf"{word}\b", text[i:], re.I) and not (text[i - 1:i].isalnum() or text[i - 1:i] == "_"):
            return i
    return -1


def where_clause(statement):
    i = keyword(statement, "WHERE")
    return None if i < 0 else statement[i + 5:].rstrip().rstrip(";")


def set_columns(statement):
    """The lower-case columns an UPDATE's SET assigns; empty if unreadable."""
    start = keyword(statement, "SET")
    if start < 0:
        return set()
    end = keyword(statement, "WHERE", start)
    clause = statement[start + 3:end if end >= 0 else len(statement)].strip().rstrip(";")
    cuts = [-1] + [i for i in top_level(clause) if clause[i] == ","] + [len(clause)]
    columns = set()
    for a, b in zip(cuts, cuts[1:]):
        target = clause[a + 1:b].split("=", 1)[0].strip().strip("()")
        names = [n.strip().strip('"').lower() for n in target.split(",")]
        if not all(re.fullmatch(r"\w+", n) for n in names):
            return set()
        columns.update(names)
    return columns


def main():
    before, after, files = sys.argv[1], sys.argv[2], sys.argv[3:]
    db = sqlite3.connect(f"file:{after}?mode=ro", uri=True)
    db.execute("ATTACH ? AS ref", (f"file:{before}?mode=ro",))
    key_of = {}

    def key(table):
        if table not in key_of:
            pk = [r[1] for r in sorted(db.execute(f'PRAGMA ref.table_info("{table}")'), key=lambda r: r[5]) if r[5]]
            key_of[table] = ", ".join(f'"{c}"' for c in pk) or "rowid"
        return key_of[table]

    updated, deleted, inserted, notes = {}, {}, set(), []
    for path in files:
        name = path.rsplit("/", 1)[-1].split("_", 1)[0]
        for statement in statements(open(path).read()):
            m = WRITE.match(statement)
            if not m:
                continue
            verb, table = m.group(1).split()[0].upper(), m.group(2)
            if verb == "INSERT":
                inserted.add(table)
                continue
            if db.execute("SELECT count(*) FROM ref.sqlite_master WHERE type = 'table' AND name = ?", (table,)).fetchone()[0] == 0:
                continue  # a table the store before does not have yet
            where = where_clause(statement)
            query = f'SELECT {key(table)} FROM ref."{table}"' + (f" WHERE {where}" if where else "")
            try:
                rows = set(db.execute(query))
            except sqlite3.Error as e:
                print(f"{name}: cannot evaluate the WHERE of {verb} {table} on the store before: {e}")
                return 1
            if verb == "UPDATE":
                columns = set_columns(statement)
                for row in rows:
                    updated.setdefault(table, {}).setdefault(row, set()).update(columns)
            else:
                deleted.setdefault(table, set()).update(rows)
            notes.append(f"{name} {verb} {table} selects {len(rows)}")

    bad = False
    tables = db.execute("SELECT name FROM ref.sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name").fetchall()
    total = 0
    for (table,) in tables:
        names = [r[1] for r in db.execute(f'PRAGMA ref.table_info("{table}")')]
        cols = ", ".join(f'"{c}"' for c in names)
        n = key(table).count(",") + 1
        try:
            old = {r[:n]: r[n:] for r in db.execute(f'SELECT {key(table)}, {cols} FROM ref."{table}"')}
            new = {r[:n]: r[n:] for r in db.execute(f'SELECT {key(table)}, {cols} FROM main."{table}"')}
        except sqlite3.Error as e:
            print(f"{table}: {e}")
            bad = True
            continue
        total += len(old)
        gone = [k for k in old if k not in new]
        changed = [k for k in old if k in new and new[k] != old[k]]
        added = [k for k in new if k not in old]
        def assigned(k):
            differ = {names[i].lower() for i, (a, b) in enumerate(zip(old[k], new[k])) if a != b}
            return differ <= updated.get(table, {}).get(k, set())
        unexplained = (any(k not in deleted.get(table, ()) for k in gone) or
                       not all(assigned(k) for k in changed) or
                       (added and table not in inserted))
        if unexplained:
            print(f"{table}: {len(gone)} rows gone, {len(changed)} changed, {len(added)} added; "
                  "not all of them made by a migration statement")
            bad = True
        elif gone or changed or added:
            print(f"{table}: {len(gone)} rows gone, {len(changed)} changed, {len(added)} added by its migrations")
    for (index,) in db.execute("SELECT name FROM ref.sqlite_master WHERE type = 'index' AND sql IS NOT NULL"):
        if db.execute("SELECT count(*) FROM main.sqlite_master WHERE type = 'index' AND name = ?", (index,)).fetchone()[0] != 1:
            print(f"index {index} is missing")
            bad = True
    for note in notes:
        print(note)
    print(f"compared {len(tables)} tables, {total} rows")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
