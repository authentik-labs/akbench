"""Unit tests for the parts of seed.py that need no authentik deployment.

Run with `make seeder-test`."""

import itertools
import json
import unittest
from unittest import mock

import seed

TINY = {"seed": 7, "users": 50, "groups": 20, "group_depth": 2, "groups_per_user": 3}


def depth_of(parents, index):
    depth = 1
    while parents[index] is not None:
        index = parents[index]
        depth += 1
    return depth


class ParseParamsTest(unittest.TestCase):
    def test_accepts_complete_params(self):
        self.assertEqual(seed.parse_params(json.dumps(TINY)), TINY)

    def test_rejects_bad_input(self):
        cases = {
            "not json": "{",
            "not an object": "[1]",
            "missing param": json.dumps({k: v for k, v in TINY.items() if k != "users"}),
            "unknown param": json.dumps({**TINY, "aps": 5}),
            "fractional": json.dumps({**TINY, "users": 1.5}),
            "boolean": json.dumps({**TINY, "users": True}),
            "string": json.dumps({**TINY, "users": "50"}),
            "no users": json.dumps({**TINY, "users": 0}),
            "negative": json.dumps({**TINY, "groups": -1}),
            "depth zero": json.dumps({**TINY, "group_depth": 0}),
            "more memberships than groups": json.dumps({**TINY, "groups_per_user": 21}),
            "deeper than groups": json.dumps({**TINY, "groups": 3, "groups_per_user": 3, "group_depth": 4}),
            "a tree without groups": json.dumps({**TINY, "groups": 0, "groups_per_user": 0}),
            "a millionth user": json.dumps({**TINY, "users": 1_000_001}),
            "a millionth group": json.dumps({**TINY, "groups": 1_000_001}),
        }
        for name, raw in cases.items():
            with self.subTest(name), self.assertRaises(ValueError):
                seed.parse_params(raw)

    def test_largest_sizes_keep_six_digit_names(self):
        params = seed.parse_params(json.dumps({**TINY, "users": 1_000_000, "groups": 1_000_000}))
        self.assertEqual(seed.username(params["users"] - 1), "u999999")
        self.assertEqual(seed.group_name(params["groups"] - 1), "g999999")


class LaterParamsTest(unittest.TestCase):
    """A dimension added later must not invalidate numbers saved, or
    databases seeded, before it existed."""

    def setUp(self):
        patch = mock.patch.dict(seed.LATER_PARAMS, {"sessions": 0})
        patch.start()
        self.addCleanup(patch.stop)

    def test_left_out_means_zero(self):
        self.assertEqual(seed.parse_params(json.dumps(TINY)), {**TINY, "sessions": 0})

    def test_can_be_set(self):
        params = seed.parse_params(json.dumps({**TINY, "sessions": 30}))
        self.assertEqual(params["sessions"], 30)

    def test_is_validated_like_the_others(self):
        with self.assertRaises(ValueError):
            seed.parse_params(json.dumps({**TINY, "sessions": -1}))

    def test_database_seeded_before_it_existed_still_matches(self):
        self.assertTrue(seed.same_tenant(TINY, {**TINY, "sessions": 0}))

    def test_asking_for_it_on_such_a_database_is_a_different_tenant(self):
        self.assertFalse(seed.same_tenant(TINY, {**TINY, "sessions": 30}))

    def test_left_at_zero_it_is_not_recorded(self):
        params = seed.parse_params(json.dumps(TINY))
        self.assertEqual(seed.recorded_params(params), TINY)
        self.assertEqual(seed.recorded_params({**TINY, "sessions": 30}), {**TINY, "sessions": 30})

    def test_a_seeder_from_before_it_existed_recognises_the_tenant(self):
        recorded = seed.recorded_params(seed.parse_params(json.dumps(TINY)))
        with mock.patch.dict(seed.LATER_PARAMS, clear=True):
            self.assertTrue(seed.same_tenant(recorded, seed.parse_params(json.dumps(TINY))))

    def test_a_zero_recorded_by_an_earlier_version_still_matches(self):
        self.assertTrue(seed.same_tenant({**TINY, "sessions": 0}, {**TINY, "sessions": 0}))
        self.assertTrue(seed.same_tenant({**TINY, "sessions": 0}, TINY))


class SameTenantTest(unittest.TestCase):
    def test_same_and_different(self):
        self.assertTrue(seed.same_tenant(dict(TINY), TINY))
        self.assertFalse(seed.same_tenant({**TINY, "users": 60}, TINY))
        self.assertFalse(seed.same_tenant(None, TINY))


class GroupTreeTest(unittest.TestCase):
    def test_flat_list_has_no_parents(self):
        params = {**TINY, "group_depth": 1}
        self.assertEqual(seed.group_parents(params), [None] * 20)

    def test_no_groups(self):
        self.assertEqual(seed.group_parents({**TINY, "groups": 0, "groups_per_user": 0}), [])

    def test_tree_has_one_root_and_the_requested_depth(self):
        cases = [(20, 2), (100, 3), (400, 3), (2000, 4), (5000, 5), (2, 5), (1, 3)]
        # Where a tree with the same number of children under every group
        # would be shallower than asked for.
        cases += [(100, 10), (11, 10), (30, 6), (10, 10), (3, 3), (1000000, 10)]
        for groups, depth in cases:
            with self.subTest(groups=groups, depth=depth):
                parents = seed.group_parents({**TINY, "groups": groups, "group_depth": depth})
                self.assertEqual(len(parents), groups)
                self.assertEqual(parents.count(None), 1)
                self.assertIsNone(parents[0])
                # A parent always comes before its children, so no cycles.
                self.assertTrue(all(p < i for i, p in enumerate(parents) if p is not None))
                deepest = max(depth_of(parents, index) for index in range(groups))
                self.assertEqual(deepest, min(depth, groups))
                # Filled level by level: no group is shallower than one
                # that comes before it.
                depths = [depth_of(parents, index) for index in range(groups)]
                self.assertEqual(depths, sorted(depths))

    def test_full_levels_keep_their_shape(self):
        """Where filling every level completely already reaches the depth,
        the tree is the plain one with the same number of children under
        every group, as in the built-in presets."""
        for groups, depth in [(20, 2), (100, 3), (400, 3), (2000, 4), (5000, 5), (1000, 3)]:
            with self.subTest(groups=groups, depth=depth):
                children = seed.branching(groups, depth)
                plain = [None] + [(index - 1) // children for index in range(1, groups)]
                self.assertEqual(seed.group_parents({**TINY, "groups": groups, "group_depth": depth}), plain)


class MembershipTest(unittest.TestCase):
    def test_each_user_gets_distinct_groups(self):
        rng = seed.stream(TINY, "memberships")
        weights = seed.membership_weights(20)
        for _ in range(200):
            groups = seed.user_groups(rng, weights, 3)
            self.assertEqual(len(groups), 3)
            self.assertEqual(len(set(groups)), 3)
            self.assertTrue(all(0 <= g < 20 for g in groups))

    def test_every_group_when_count_equals_groups(self):
        rng = seed.stream(TINY, "memberships")
        self.assertEqual(
            sorted(seed.user_groups(rng, seed.membership_weights(8), 8)), list(range(8))
        )

    def test_first_groups_are_the_largest(self):
        rng = seed.stream(TINY, "memberships")
        weights = seed.membership_weights(100)
        sizes = [0] * 100
        for _ in range(5000):
            for group in seed.user_groups(rng, weights, 5):
                sizes[group] += 1
        self.assertGreater(sizes[0], 5 * sizes[50])
        self.assertGreater(min(sizes), 0)


class DeterminismTest(unittest.TestCase):
    def test_same_seed_same_data(self):
        def draw(params):
            uuids = seed.stream(params, "users")
            memberships = seed.stream(params, "memberships")
            weights = seed.membership_weights(params["groups"])
            return (
                [str(seed.new_uuid(uuids)) for _ in range(10)],
                [
                    seed.user_groups(memberships, weights, params["groups_per_user"])
                    for _ in range(10)
                ],
            )

        self.assertEqual(draw(TINY), draw(dict(TINY)))
        self.assertNotEqual(draw(TINY), draw({**TINY, "seed": 8}))

    def test_streams_are_independent(self):
        # More groups must not change which UUIDs the users get.
        first = str(seed.new_uuid(seed.stream(TINY, "users")))
        second = str(seed.new_uuid(seed.stream({**TINY, "groups": 400}, "users")))
        self.assertEqual(first, second)

    def test_uuids_are_valid_version_4(self):
        value = seed.new_uuid(seed.stream(TINY, "users"))
        self.assertEqual(value.version, 4)


class PinnedValuesTest(unittest.TestCase):
    """The data fixed params produce, written out. Comparisons and reruns
    depend on it never changing, across Python versions as much as across
    changes to this file: a change here means existing tenants no longer
    match what the seeder would create."""

    def test_users(self):
        self.assertEqual(
            [str(value) for value in itertools.islice(seed.user_uuids(TINY), 3)],
            [
                "72bef7ce-10b6-4c4f-a47d-bb3c07dc0077",
                "ee8247b8-8401-4c86-b714-dc842ead58c4",
                "5b8a5bee-75a2-42c0-a0ec-76ef17f98b5f",
            ],
        )

    def test_groups(self):
        self.assertEqual(
            [str(value) for value in seed.group_uuids(TINY)[:3]],
            [
                "202bce2a-1cba-470c-8d73-14b52b105f2e",
                "6e68db5a-3a36-4fad-aa52-1cf309e4e601",
                "44a77c44-375e-4a42-8310-cb60b9cc7264",
            ],
        )

    def test_memberships(self):
        self.assertEqual(
            list(itertools.islice(seed.user_memberships(TINY), 3)),
            [[0, 4, 13], [19, 1, 5], [2, 3, 0]],
        )
        # Every group for every user: the draws plus the fill.
        every = {**TINY, "groups": 8, "groups_per_user": 8}
        self.assertEqual(
            list(itertools.islice(seed.user_memberships(every), 3)),
            [[0, 2, 5, 7, 1, 3, 4, 6], [0, 3, 1, 5, 6, 4, 2, 7], [7, 1, 4, 0, 5, 2, 6, 3]],
        )

    def test_group_parents(self):
        # Deeper than full levels allow, so groups are held back for the
        # lower levels.
        self.assertEqual(
            seed.group_parents({**TINY, "groups": 30, "group_depth": 6}),
            [None, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6]
            + [7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13, 15],
        )

    def test_counts_follow_the_params(self):
        self.assertEqual(len(list(seed.user_uuids(TINY))), 50)
        self.assertEqual(len(seed.group_uuids(TINY)), 20)
        self.assertEqual(len(list(seed.user_memberships(TINY))), 50)


class CheckMarkerTest(unittest.TestCase):
    def groups(self, attributes=None):
        """A stand-in for the Group manager, holding a marker with
        attributes, or none."""
        manager = mock.Mock()
        marker = None if attributes is None else mock.Mock(attributes=attributes)
        manager.filter.return_value.first.return_value = marker
        return manager

    def done(self, params):
        return {seed.MARKER_KEY: {"state": "done", "params": params}}

    def test_unseeded(self):
        groups = self.groups()
        self.assertFalse(seed.check_marker(TINY, groups))
        groups.filter.assert_called_once_with(name=seed.MARKER_GROUP)

    def test_seeded_with_the_same_params(self):
        self.assertTrue(seed.check_marker(TINY, self.groups(self.done(dict(TINY)))))

    def test_seeded_with_other_params(self):
        with self.assertRaisesRegex(RuntimeError, "already seeded with"):
            seed.check_marker(TINY, self.groups(self.done({**TINY, "users": 60})))

    def test_left_seeding_by_an_earlier_version(self):
        attributes = {seed.MARKER_KEY: {"state": "seeding", "params": dict(TINY)}}
        with self.assertRaisesRegex(RuntimeError, "did not finish"):
            seed.check_marker(TINY, self.groups(attributes))

    def test_marker_without_a_record(self):
        with self.assertRaises(RuntimeError):
            seed.check_marker(TINY, self.groups({}))


class MainTest(unittest.TestCase):
    def run_main(self, environ, error=None):
        with (
            mock.patch.dict(seed.os.environ, environ, clear=True),
            mock.patch.object(seed, "seed", side_effect=error) as seed_mock,
            self.assertRaises(SystemExit) as raised,
        ):
            seed.main()
        return str(raised.exception.code), seed_mock

    def test_every_failure_is_prefixed(self):
        good = {"TENANT_PARAMS": json.dumps(TINY), "TENANT_PASSWORD": "secret"}
        cases = {
            "no password": ({"TENANT_PARAMS": json.dumps(TINY)}, None),
            "bad params": ({**good, "TENANT_PARAMS": "{"}, None),
            "the seeder's own error": (good, RuntimeError("reset the deployment")),
            "a database error": (good, OSError("connection refused")),
            "a missing module": (good, ImportError("no module named pgtrigger")),
        }
        for name, (environ, error) in cases.items():
            with self.subTest(name):
                message, _ = self.run_main(environ, error)
                self.assertTrue(message.startswith("seeding failed: "), message)

    def test_messages(self):
        message, seed_mock = self.run_main({"TENANT_PARAMS": json.dumps(TINY)})
        self.assertEqual(message, "seeding failed: TENANT_PASSWORD is required")
        seed_mock.assert_not_called()
        good = {"TENANT_PARAMS": json.dumps(TINY), "TENANT_PASSWORD": "secret"}
        message, _ = self.run_main(good, OSError("connection refused"))
        self.assertEqual(message, "seeding failed: OSError: connection refused")

    def test_success_does_not_exit(self):
        environ = {"TENANT_PARAMS": json.dumps(TINY), "TENANT_PASSWORD": "secret"}
        with (
            mock.patch.dict(seed.os.environ, environ, clear=True),
            mock.patch.object(seed, "seed") as seed_mock,
        ):
            seed.main()
        seed_mock.assert_called_once_with(TINY, "secret")


class SupportTest(unittest.TestCase):
    def test_missing_fields_are_named(self):
        class Meta:
            def get_field(self, name):
                if name not in ("user", "name"):
                    raise LookupError(name)

        class Model:
            _meta = Meta()

        self.assertEqual(seed.missing_fields(Model, ("user", "name")), [])
        self.assertEqual(seed.missing_fields(Model, ("user", "name", "password")), ["password"])

    def loader(self, models):
        """A stand-in for load_model over a build holding models, a dict of
        dotted path to field names."""

        def load(path):
            if path not in models:
                raise AttributeError(path)

            class Meta:
                def get_field(self, name):
                    if name not in models[path]:
                        raise LookupError(name)

            return type("Model", (), {"_meta": Meta()})

        return load

    def test_password_storage(self):
        device = self.loader({seed.PASSWORD_DEVICE: ("user", "name", "password")})
        self.assertEqual(seed.password_storage(device), "device")
        self.assertEqual(seed.password_storage(self.loader({})), "user")

    def test_model_problems(self):
        user = "authentik.core.models.User"
        load = self.loader({user: ("password",)})
        self.assertEqual(
            seed.model_problems(seed.PASSWORD_STORAGE["user"], load),
            ["User has no field password_change_date"],
        )
        self.assertEqual(
            seed.model_problems(seed.PASSWORD_STORAGE["device"], load),
            [f"no model {seed.PASSWORD_DEVICE}"],
        )


class NamingTest(unittest.TestCase):
    def test_names_sort_and_stay_unique_up_to_the_largest_size(self):
        self.assertEqual(seed.username(0), "u000000")
        self.assertEqual(seed.username(499999), "u499999")
        self.assertEqual(seed.group_name(4999), "g004999")

    def test_user_fields(self):
        fields = seed.user_fields(19)
        self.assertEqual(fields["email"], "u000019@tenant.example")
        self.assertEqual(fields["path"], "users/dept-19")
        self.assertTrue(fields["external"])
        self.assertFalse(seed.user_fields(0)["external"])

    def test_batches_cover_everything_once(self):
        self.assertEqual(seed.batches(0), [])
        self.assertEqual(seed.batches(3, 5), [(0, 3)])
        self.assertEqual(seed.batches(12, 5), [(0, 5), (5, 10), (10, 12)])


if __name__ == "__main__":
    unittest.main()
