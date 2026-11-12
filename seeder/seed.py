"""Seed an authentik deployment with a synthetic tenant for benchmarking.

Runs inside the deployment's own authentik image, through its shell command,
so it uses that build's models and follows whatever schema it has:

    TENANT_PARAMS='{"seed": 1, "users": 50, ...}' TENANT_PASSWORD=... ak shell -c "$(cat seed.py)"

(Passed with -c rather than on stdin: the shell command only reads a script
from stdin if it is already there when it looks, and otherwise opens an
interactive prompt.)

TENANT_PARAMS is a JSON object of plain numbers (see PARAMS and LATER_PARAMS
below). The script knows nothing about named sizes: a size is just a saved set
of these numbers, and every number can be set independently. A step whose
count is 0 is skipped.

The same params always produce the same tenant - the same user and group
names, user and group UUIDs, attributes, group tree and memberships - so the
deployments of one comparison hold the same data without copying a database
between them. Not reproduced are timestamps and the keys nothing outside one
database refers to: the users' integer primary keys, the keys of the membership and
password rows, and the random UUIDs of the rows linking a group to its parent.

A marker records the params a database was seeded with. Seeding again with
the same params does nothing; with different params it fails rather than
topping the data up: a deployment's data must match exactly one set of
numbers. Reset the deployment to seed it differently.

A run writes everything, the marker included, in one transaction, under a
lock that a second run waits for. A run that fails leaves nothing behind and
can simply be retried; a second run started alongside it waits, then finds
the tenant already seeded.

Every failure ends the script with a single line starting "seeding failed: ".

The first half of this file is plain Python with no authentik imports, so it
can be unit-tested without a deployment (test_seed.py).
"""

import json
import os
import random
import uuid
from collections.abc import Iterator
from importlib import import_module

# The original tenant dimensions, with the smallest value each accepts. All of
# them must be given: the size of a tenant is part of what a result measured
# against it means, so none is assumed.
PARAMS = {
    # Makes the tenant reproducible: same seed, same data.
    "seed": 0,
    "users": 1,
    "groups": 0,
    # Levels in the group tree: 1 is a flat list of groups, 2 is one root
    # group with every other group below it, and so on. At most groups.
    "group_depth": 1,
    # Groups each user is a direct member of.
    "groups_per_user": 0,
}

# The largest value a param accepts, where there is one. Names carry six
# digits (u000000 to u999999), so a user or group past the millionth would
# break the naming contract (see username).
MAXIMUMS = {"users": 1_000_000, "groups": 1_000_000}

# Dimensions added after the first version go here, not in PARAMS. They may be
# left out and then count as 0, which skips their step. That keeps a set of
# numbers saved before a dimension existed valid, a database seeded before it
# existed recognised as seeded (see same_tenant), and one seeded with it left
# at 0 recognised by a seeder from before it existed (see recorded_params).
LATER_PARAMS: dict[str, int] = {}

# Rows per insert, and so the most rows built in memory before one.
BATCH_SIZE = 5000

# The group recording what a database was seeded with. It is not part of the
# group tree and has no members.
MARKER_GROUP = "akbench-tenant-marker"
MARKER_KEY = "akbench_tenant"

# Every authentik model the seeder writes to, with the fields it sets. Checked
# against the build before anything is written (check_supported), so a build
# that renamed or dropped one is reported by name instead of failing midway.
# The models the password goes to depend on the build: see PASSWORD_STORAGE.
MODELS = {
    "authentik.core.models.Group": ("group_uuid", "name", "attributes"),
    "authentik.core.models.GroupParentageNode": ("child", "parent"),
    "authentik.core.models.User": (
        "uuid",
        "username",
        "name",
        "email",
        "path",
        "type",
        "attributes",
        "is_active",
    ),
    "authentik.core.models.UserGroup": ("user", "group"),
}

# Where a build keeps password hashes, with the fields the seeder sets there.
# Newer builds keep each user's hash on a PasswordDevice; releases up to
# 2026.8 keep it on the user row, as Django does, next to the date it was
# set. A build has PasswordDevice or uses the user row (password_storage).
PASSWORD_DEVICE = "authentik.stages.password.models.PasswordDevice"
PASSWORD_STORAGE = {
    "device": {PASSWORD_DEVICE: ("user", "name", "password")},
    "user": {"authentik.core.models.User": ("password", "password_change_date")},
}

ANCESTRY_VIEW = "authentik_core_groupancestry"
ANCESTRY_TRIGGER = "authentik_core.GroupParentageNode:refresh_groupancestry"

# The Postgres advisory lock a run holds for its transaction, so two runs
# against one database seed it once. Any fixed number works as long as
# nothing else in authentik uses it; this one is "akbench" in ASCII.
LOCK_KEY = 0x616B62656E6368

DEPARTMENTS = 20
# One user in this many is of the "external" type, like a contractor.
EXTERNAL_EVERY = 20


def parse_params(raw: str) -> dict[str, int]:
    """Validate TENANT_PARAMS and return it as a dict of ints.

    Every param in PARAMS must be given; one in LATER_PARAMS defaults to 0."""
    try:
        params = json.loads(raw)
    except ValueError as exc:
        raise ValueError(f"TENANT_PARAMS is not valid JSON: {exc}") from exc
    if not isinstance(params, dict):
        raise ValueError("TENANT_PARAMS must be a JSON object")
    known = {**PARAMS, **LATER_PARAMS}
    unknown = sorted(set(params) - set(known))
    if unknown:
        raise ValueError(f"unknown tenant params: {', '.join(unknown)}")
    missing = sorted(set(PARAMS) - set(params))
    if missing:
        raise ValueError(f"missing tenant params: {', '.join(missing)}")
    params = {**dict.fromkeys(LATER_PARAMS, 0), **params}
    for key, minimum in known.items():
        value = params[key]
        if isinstance(value, bool) or not isinstance(value, int):
            raise ValueError(f"tenant param {key} must be a whole number, got {value!r}")
        if value < minimum:
            raise ValueError(f"tenant param {key} must be at least {minimum}, got {value}")
        maximum = MAXIMUMS.get(key)
        if maximum is not None and value > maximum:
            raise ValueError(f"tenant param {key} must be at most {maximum}, got {value}")
    if params["group_depth"] > max(params["groups"], 1):
        raise ValueError(
            f"group_depth ({params['group_depth']}) cannot exceed "
            f"groups ({params['groups']}): each level needs a group"
        )
    if params["groups_per_user"] > params["groups"]:
        raise ValueError(
            f"groups_per_user ({params['groups_per_user']}) cannot exceed "
            f"groups ({params['groups']})"
        )
    return params


def recorded_params(params: dict) -> dict:
    """params as the marker records them: without the later dimensions left
    at 0. A seeder from before such a dimension existed knows nothing of it,
    so it would read a recorded 0 as a different tenant; nothing was created
    for the dimension, so the tenant is the same."""
    return {key: value for key, value in params.items() if not (key in LATER_PARAMS and value == 0)}


def same_tenant(recorded: object, params: dict[str, int]) -> bool:
    """Whether the params recorded when a database was seeded describe the
    same tenant as params. A dimension that did not exist when the database
    was seeded counts as 0 there, since nothing was created for it; one
    recorded as 0 by an earlier version of this seeder counts the same."""
    if not isinstance(recorded, dict):
        return False
    return recorded_params(recorded) == recorded_params(params)


def stream(params: dict[str, int], purpose: str) -> random.Random:
    """A random stream of its own for one part of the tenant, so adding a
    step later, or changing one count, does not shift the data of the others."""
    return random.Random(f"{params['seed']}/{purpose}")


def new_uuid(rng: random.Random) -> uuid.UUID:
    return uuid.UUID(int=rng.getrandbits(128), version=4)


def group_uuids(params: dict[str, int]) -> list[uuid.UUID]:
    """The primary keys of the groups, in order."""
    rng = stream(params, "groups")
    return [new_uuid(rng) for _ in range(params["groups"])]


def user_uuids(params: dict[str, int]) -> Iterator[uuid.UUID]:
    """The UUIDs of the users, in order."""
    rng = stream(params, "users")
    for _ in range(params["users"]):
        yield new_uuid(rng)


def username(index: int) -> str:
    """The username of the index-th user, counting from 0. The names are a
    contract: k6 scripts derive them from an index instead of reading a
    credentials file, and blueprints look seeded users up by them."""
    return f"u{index:06d}"


def group_name(index: int) -> str:
    """The name of the index-th group. Like usernames, a contract: blueprints
    bind applications to seeded groups by name."""
    return f"g{index:06d}"


def branching(groups: int, depth: int) -> int:
    """The smallest number of children per group that fits `groups` groups
    into a tree `depth` levels deep with a single root."""
    if depth <= 1 or groups <= 1:
        return 0
    children = 1
    while sum(children**level for level in range(depth)) < groups:
        children += 1
    return children


def level_sizes(groups: int, depth: int, children: int) -> list[int]:
    """How many groups each level of the tree holds.

    Each level is filled up to `children` groups per group above it, except
    that enough groups are held back for every level still to come: filling
    every level completely can use up the groups before the tree is `depth`
    levels deep (100 groups, 10 levels, 2 children each would stop at 7)."""
    sizes = [1]
    left = groups - 1
    for level in range(1, depth):
        size = min(sizes[-1] * children, left - (depth - 1 - level))
        sizes.append(size)
        left -= size
    return sizes


def group_parents(params: dict[str, int]) -> list[int | None]:
    """The parent of each group as an index into the group list, or None.

    Groups form one tree, exactly group_depth levels deep (or as deep as the
    groups allow), filled level by level, so group 0 is the root that every
    user is transitively a member of and low-numbered groups have the most
    groups below them. With group_depth 1 there is no tree, only a flat list."""
    groups = params["groups"]
    depth = min(params["group_depth"], groups)
    children = branching(groups, depth)
    if children == 0:
        return [None] * groups
    parents: list[int | None] = [None]
    level_start = 0
    for size in level_sizes(groups, depth, children)[1:]:
        parents += [level_start + index // children for index in range(size)]
        level_start = len(parents) - size
    return parents


def group_attributes(index: int) -> dict:
    """Attributes for the index-th group. One group in five carries some, so
    merging a user's group attributes has something to merge."""
    if index % 5:
        return {}
    return {"cost_center": f"CC-{index % 97:04d}", "settings": {"theme": {"base": "automatic"}}}


def user_fields(index: int) -> dict:
    """The model-independent fields of the index-th user."""
    department = index % DEPARTMENTS
    return {
        "username": username(index),
        "name": f"Tenant User {index:06d}",
        "email": f"{username(index)}@tenant.example",
        "path": f"users/dept-{department:02d}",
        "external": index % EXTERNAL_EVERY == EXTERNAL_EVERY - 1,
        "attributes": {
            "employee_number": f"E{100000 + index}",
            "department": f"dept-{department:02d}",
            "title": ("Engineer", "Analyst", "Manager", "Specialist")[index % 4],
        },
    }


def membership_weights(groups: int) -> list[float]:
    """Cumulative weights that make a few groups large and most small, the
    way real directories are: the n-th group is picked about 1/n as often as
    the first."""
    total = 0.0
    weights = []
    for index in range(groups):
        total += 1.0 / (index + 1)
        weights.append(total)
    return weights


def user_groups(rng: random.Random, weights: list[float], count: int) -> list[int]:
    """`count` distinct group indexes for one user, in the order drawn."""
    picked = {}
    population = range(len(weights))
    # Draws repeat, more so when count is close to the number of groups.
    # The cap keeps a pathological case from looping forever; the fill below
    # then takes the first unused groups.
    for _ in range(50):
        if len(picked) >= count:
            break
        for index in rng.choices(population, cum_weights=weights, k=count - len(picked)):
            picked.setdefault(index, None)
    for index in population:
        if len(picked) >= count:
            break
        picked.setdefault(index, None)
    return list(picked)[:count]


def user_memberships(params: dict[str, int]) -> Iterator[list[int]]:
    """The groups each user is a direct member of, one list per user, in
    user order. Drawn one user at a time, so it can be consumed in pieces of
    any size without holding all the memberships at once."""
    rng = stream(params, "memberships")
    weights = membership_weights(params["groups"])
    for _ in range(params["users"]):
        yield user_groups(rng, weights, params["groups_per_user"])


def missing_fields(model: type, fields: tuple[str, ...]) -> list[str]:
    """The names in fields that the model does not have."""
    missing = []
    for field in fields:
        try:
            model._meta.get_field(field)
        except Exception:  # Django's FieldDoesNotExist; not imported, to keep this half Django-free
            missing.append(field)
    return missing


def load_model(path: str) -> type:
    """The class at a dotted path. Raises ImportError or AttributeError if
    the build has no such module or class."""
    module, _, name = path.rpartition(".")
    return getattr(import_module(module), name)


def model_problems(models: dict[str, tuple[str, ...]], load=load_model) -> list[str]:
    """What is missing from a build for models, by name."""
    problems = []
    for path, fields in models.items():
        try:
            model = load(path)
        except (ImportError, AttributeError):
            problems.append(f"no model {path}")
            continue
        name = path.rpartition(".")[2]
        problems += [f"{name} has no field {field}" for field in missing_fields(model, fields)]
    return problems


def password_storage(load=load_model) -> str:
    """Where this build keeps password hashes: "device" or "user", a key of
    PASSWORD_STORAGE."""
    try:
        load(PASSWORD_DEVICE)
    except (ImportError, AttributeError):
        return "user"
    return "device"


def check_marker(params: dict[str, int], groups) -> bool:
    """Report whether this database is already seeded with params, given
    the manager (or queryset) to read groups through.

    Returns True when it is (nothing to do), False when it is unseeded, and
    raises when it holds a different tenant. A marker still in the "seeding"
    state was left by an earlier version of this seeder, which committed as
    it went: that database holds part of a tenant."""
    marker = groups.filter(name=MARKER_GROUP).first()
    if marker is None:
        return False
    recorded = marker.attributes.get(MARKER_KEY, {})
    if recorded.get("state") != "done":
        raise RuntimeError(
            "this database holds a tenant whose seeding did not finish; "
            "reset the deployment and seed again"
        )
    if not same_tenant(recorded.get("params"), params):
        raise RuntimeError(
            "this database is already seeded with "
            f"{json.dumps(recorded_params(recorded.get('params') or {}), sort_keys=True)}, "
            f"not {json.dumps(recorded_params(params), sort_keys=True)}; "
            "reset the deployment to seed it differently"
        )
    return True


def batches(total: int, size: int = BATCH_SIZE) -> list[tuple[int, int]]:
    """(start, end) index ranges covering 0..total in steps of size."""
    return [(start, min(start + size, total)) for start in range(0, total, size)]


def failure_message(exc: BaseException) -> str:
    """The line a failed run ends with. The seeder's own errors explain
    themselves; anything else (a database error, a missing module) is named
    by its type, since its message alone often is not enough."""
    if isinstance(exc, (ValueError, RuntimeError)):
        return f"seeding failed: {exc}"
    return f"seeding failed: {type(exc).__name__}: {exc}"


# -- Everything below needs authentik's models and a database.


def check_supported() -> str:
    """Fail before touching anything if this authentik build lacks a model,
    field, trigger or view the seeder relies on, naming what is missing.
    Returns where the build keeps password hashes (password_storage)."""
    import pgtrigger
    from django.db import DEFAULT_DB_ALIAS, connections

    storage = password_storage()
    problems = model_problems(MODELS) + model_problems(PASSWORD_STORAGE[storage])
    try:
        pgtrigger.registered(ANCESTRY_TRIGGER)
    except (KeyError, ValueError):
        problems.append(f"no trigger {ANCESTRY_TRIGGER}")
    with connections[DEFAULT_DB_ALIAS].cursor() as cursor:
        cursor.execute("SELECT 1 FROM pg_matviews WHERE matviewname = %s", [ANCESTRY_VIEW])
        if cursor.fetchone() is None:
            problems.append(f"no materialized view {ANCESTRY_VIEW}")
    if problems:
        raise RuntimeError(
            "the seeder does not support this authentik build and has to be updated: "
            + "; ".join(problems)
        )
    return storage


def check_migrated() -> None:
    """Fail before touching anything if authentik is still migrating: a
    half-migrated database does not have the schema the seeder checked."""
    from django.db import DEFAULT_DB_ALIAS, connections
    from django.db.migrations.executor import MigrationExecutor

    executor = MigrationExecutor(connections[DEFAULT_DB_ALIAS])
    if executor.migration_plan(executor.loader.graph.leaf_nodes()):
        raise RuntimeError(
            "this database's migrations have not finished; wait for authentik to start, then seed"
        )


def seed_groups(params: dict[str, int]) -> list[uuid.UUID]:
    """Create the groups and their tree. Returns their primary keys in order.

    Leaves the ancestry view stale: the caller refreshes it (refresh_ancestry)
    once everything else is written."""
    import pgtrigger
    from django.db import DEFAULT_DB_ALIAS

    from authentik.core.models import Group, GroupParentageNode

    pks = group_uuids(params)
    for start, end in batches(len(pks)):
        Group.objects.bulk_create(
            Group(group_uuid=pks[index], name=group_name(index), attributes=group_attributes(index))
            for index in range(start, end)
        )

    edges = [
        (index, parent) for index, parent in enumerate(group_parents(params)) if parent is not None
    ]
    # The table's trigger refreshes the whole ancestry view after every
    # row, which would make loading the tree quadratic. Skip it for the
    # load and refresh once.
    with pgtrigger.ignore(ANCESTRY_TRIGGER, databases=[DEFAULT_DB_ALIAS]):
        for start, end in batches(len(edges)):
            GroupParentageNode.objects.bulk_create(
                GroupParentageNode(child_id=pks[child], parent_id=pks[parent])
                for child, parent in edges[start:end]
            )
    return pks


def refresh_ancestry() -> None:
    """Rebuild the group ancestry view from the group tree.

    Runs in the seeding transaction, as authentik's own trigger does, so the
    view sees the uncommitted tree and is never committed without it. The
    refresh locks the view against readers until the commit, which is why it
    comes last."""
    from django.db import DEFAULT_DB_ALIAS, connections

    with connections[DEFAULT_DB_ALIAS].cursor() as cursor:
        cursor.execute(f"REFRESH MATERIALIZED VIEW {ANCESTRY_VIEW}")


def seed_users(
    params: dict[str, int], password_hash: str, storage: str, group_pks: list[uuid.UUID]
) -> None:
    """Create the users, their passwords and their group memberships.

    Every user gets password_hash, stored where the build keeps passwords
    (storage, see PASSWORD_STORAGE)."""
    from django.utils.timezone import now

    from authentik.core.models import User, UserGroup, UserTypes

    if storage == "device":
        from authentik.stages.password.models import PasswordDevice

        password_fields = {}
    else:
        # What User.set_password_from_hash sets on these builds.
        password_fields = {"password": password_hash, "password_change_date": now()}

    uuids = user_uuids(params)
    memberships = user_memberships(params)
    for start, end in batches(params["users"]):
        users = []
        for index in range(start, end):
            fields = user_fields(index)
            users.append(
                User(
                    uuid=next(uuids),
                    username=fields["username"],
                    name=fields["name"],
                    email=fields["email"],
                    path=fields["path"],
                    type=UserTypes.EXTERNAL if fields["external"] else UserTypes.INTERNAL,
                    attributes=fields["attributes"],
                    is_active=True,
                    **password_fields,
                )
            )
        users = User.objects.bulk_create(users)
        if storage == "device":
            PasswordDevice.objects.bulk_create(
                PasswordDevice(user_id=user.pk, name="Password", password=password_hash)
                for user in users
            )
        if not params["groups_per_user"]:
            continue
        # Flushed every BATCH_SIZE memberships rather than once per batch
        # of users: with many groups per user, one batch of users can have
        # millions of memberships.
        pending = []
        for user in users:
            for group in next(memberships):
                pending.append(UserGroup(user_id=user.pk, group_id=group_pks[group]))
                if len(pending) >= BATCH_SIZE:
                    UserGroup.objects.bulk_create(pending)
                    pending = []
        if pending:
            UserGroup.objects.bulk_create(pending)


def seed(params: dict[str, int], password: str) -> None:
    """Seed the tenant described by params, unless it is already there."""
    from django.contrib.auth.hashers import make_password
    from django.db import DEFAULT_DB_ALIAS, connections, transaction

    from authentik.core.models import Group, GroupParentageNode, User, UserGroup

    check_migrated()
    storage = check_supported()
    # Hashed once and shared: hashing each user's password would take hours
    # at the larger sizes. A login still pays for the full verification.
    password_hash = make_password(password)

    # Everything goes through the primary, by name: authentik can route
    # reads to a read replica, which would not see the marker another run
    # just committed, nor this transaction's own rows.
    connection = connections[DEFAULT_DB_ALIAS]
    with transaction.atomic(using=DEFAULT_DB_ALIAS):
        with connection.cursor() as cursor:
            # Held until the transaction ends. A second run waits here, and
            # only then reads the marker, so it sees what the first one
            # committed (each statement reads the latest committed data).
            cursor.execute("SELECT pg_advisory_xact_lock(%s)", [LOCK_KEY])
        if check_marker(params, Group.objects.using(DEFAULT_DB_ALIAS)):
            print(f"tenant already seeded with {json.dumps(params, sort_keys=True)}")
            return
        # Written as done straight away: it commits with the tenant or not
        # at all.
        Group.objects.using(DEFAULT_DB_ALIAS).create(
            name=MARKER_GROUP,
            attributes={MARKER_KEY: {"state": "done", "params": recorded_params(params)}},
        )
        group_pks = seed_groups(params)
        seed_users(params, password_hash, storage, group_pks)
        counts = {
            "params": params,
            "users": User.objects.using(DEFAULT_DB_ALIAS)
            .filter(email__endswith="@tenant.example")
            .count(),
            "groups": Group.objects.using(DEFAULT_DB_ALIAS).filter(name__regex=r"^g\d{6}$").count(),
            "group_parents": GroupParentageNode.objects.using(DEFAULT_DB_ALIAS).count(),
            "memberships": UserGroup.objects.using(DEFAULT_DB_ALIAS)
            .filter(user__email__endswith="@tenant.example")
            .count(),
        }
        refresh_ancestry()

    # Fresh planner statistics, so the first queries of a test are planned
    # for the data that is there now. After the commit, so it counts the
    # new rows.
    with connection.cursor() as cursor:
        cursor.execute("ANALYZE")
    print("seeded tenant: " + json.dumps(counts, sort_keys=True))


def main() -> None:
    # Everything that goes wrong, whatever its type, ends in one line the
    # caller can recognise as a failure.
    try:
        password = os.environ.get("TENANT_PASSWORD", "")
        if not password:
            raise ValueError("TENANT_PASSWORD is required")
        params = parse_params(os.environ.get("TENANT_PARAMS", ""))
        seed(params, password)
    except Exception as exc:
        raise SystemExit(failure_message(exc)) from exc


# The shell command exec's this file rather than running it as __main__, so
# the trigger is the environment: unit tests import it without TENANT_PARAMS.
if "TENANT_PARAMS" in os.environ:
    main()
