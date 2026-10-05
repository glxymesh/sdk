import json
import unittest

from glxymesh import ToolError
from glxymesh.runtime import Tool, handler


def greet(ctx, input):
    ctx.log.info("greeting " + input["name"])
    return {"greeting": f"Hello, {input['name']}", "client": ctx.caller.client_id}


def refuse(ctx, input):
    raise ToolError("name is required")


async def slow(ctx, input):
    return input["n"] * 2


def broken(ctx, input):
    return {}["missing"]


class HandlerTest(unittest.TestCase):
    def setUp(self):
        self.handle = handler(
            {
                "greet": Tool(greet, {"name": "Ada"}),
                "refuse": Tool(refuse, {}),
                "slow": Tool(slow, {}),
                "broken": Tool(broken, {}),
            },
            egress_url="",
        )

    def call(self, tool, args):
        return self.handle({"tool": tool, "argumentsJson": json.dumps(args), "caller": {"clientId": "claude"}})

    def test_a_result_and_its_log(self):
        out = self.call("greet", {})
        self.assertEqual(json.loads(out["resultJson"]), {"greeting": "Hello, Ada", "client": "claude"})
        self.assertEqual(out["logs"][0]["message"], "greeting Ada")

    def test_a_tool_error_reaches_the_model(self):
        out = self.call("refuse", {})
        self.assertEqual((out["errorKind"], out["errorMessage"]), ("ERROR_KIND_TOOL", "name is required"))

    def test_an_async_run(self):
        self.assertEqual(self.call("slow", {"n": 21})["resultJson"], "42")

    def test_a_bug_is_a_crash_with_its_traceback(self):
        out = self.call("broken", {})
        self.assertEqual(out["errorKind"], "ERROR_KIND_CRASH")
        self.assertIn("KeyError", out["logs"][0]["message"])

    def test_an_unknown_tool(self):
        self.assertEqual(self.call("nope", {})["errorKind"], "ERROR_KIND_CRASH")


if __name__ == "__main__":
    unittest.main()
