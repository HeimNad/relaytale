const { test, expect } = require("@playwright/test");
test("workspace: login, records, guarded writes, text isolation and mobile", async ({
  page,
  context,
}) => {
  const external = [];
  page.on("request", (r) => {
    if (r.url().includes("untrusted.invalid")) external.push(r.url());
  });
  await page.goto("/console/");
  await expect(
    page.getByRole("heading", { name: "登录 RelayTale" }),
  ).toBeVisible();
  await page.screenshot({
    path: "test-results/login-desktop.png",
    fullPage: true,
  });
  await page.getByLabel("账户", { exact: true }).fill("owner");
  await page
    .getByLabel("密码", { exact: true })
    .fill("browser-test-password-only");
  await page.getByRole("button", { name: "登录工作台" }).click();
  await expect(
    page.getByRole("heading", { name: "邮件记录", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "订单确认 · 需要核对投递结果" }),
  ).toBeVisible();
  await page.screenshot({
    path: "test-results/messages-desktop.png",
    fullPage: true,
  });
  await page
    .getByRole("button", { name: "订单确认 · 需要核对投递结果" })
    .click();
  await expect(page.getByRole("heading", { name: "事件时间线" })).toBeVisible();
  await page.screenshot({
    path: "test-results/detail-desktop.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "人工处置" }).first().click();
  await page.getByLabel("处理方式").selectOption("retry");
  await expect(
    page.getByText("我已核对证据，接受重新投递可能导致重复收信的风险。"),
  ).toBeVisible();
  await page
    .getByLabel("处理理由")
    .fill("浏览器验收：未接受重复风险，不提交重试");
  await page.getByRole("button", { name: "确认并记录" }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.getByLabel("处理方式").selectOption("mark-failed");
  await page.getByRole("button", { name: "确认并记录" }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await expect(
    page.getByText("永久失败", { exact: true }).first(),
  ).toBeVisible();
  await page.getByRole("link", { name: "地址抑制" }).click();
  await page.getByRole("button", { name: "添加抑制" }).click();
  const email = "browser-" + Date.now() + "@example.test";
  await page.getByLabel("邮箱地址").fill(email);
  await page.getByLabel("处理理由").fill("浏览器验证抑制");
  await page.getByRole("button", { name: "确认添加" }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page
    .getByRole("row")
    .filter({ hasText: email })
    .getByRole("button", { name: "解除" })
    .click();
  await page.getByLabel("处理理由").fill("浏览器验证解除");
  await page.getByRole("button", { name: "确认解除" }).click();
  await expect(page.getByRole("row").filter({ hasText: email })).toContainText(
    "历史记录",
  );
  await page.getByRole("link", { name: "发送通道" }).click();
  await page.getByRole("button", { name: "编辑", exact: true }).first().click();
  await page.getByLabel("通道名称").fill("浏览器验证通道");
  await page.getByLabel("处理理由").fill("浏览器验证配置版本");
  await page.getByRole("button", { name: "保存配置" }).click();
  await expect(
    page.getByRole("heading", { name: "浏览器验证通道" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "轮换密码", exact: true })
    .first()
    .click();
  await page.getByLabel("新密码").fill("browser-rotated-test-password");
  await page.getByLabel("处理理由").fill("浏览器验证密码轮换");
  await page.getByRole("button", { name: "确认轮换" }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page.getByRole("link", { name: "邮件记录" }).click();
  const malicious = page.getByRole("button", {
    name: '<img src="https://untrusted.invalid/pixel" onerror="window.injected=true">',
    exact: true,
  });
  await malicious.click();
  await expect(page.locator("#view img")).toHaveCount(0);
  expect(await page.evaluate(() => window.injected)).toBeUndefined();
  expect(external).toEqual([]);
  expect(
    await page.evaluate(() => localStorage.length + sessionStorage.length),
  ).toBe(0);
  expect(await page.evaluate(() => document.cookie)).not.toContain("session");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("link", { name: "邮件记录" }).click();
  await expect(
    page.getByRole("heading", { name: "邮件记录", exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: "test-results/messages-mobile.png",
    fullPage: true,
  });
  await page.getByRole("link", { name: "访问与说明" }).click();
  await page
    .getByRole("button", { name: "退出登录", exact: true })
    .last()
    .click();
  await expect(
    page.getByRole("heading", { name: "登录 RelayTale" }),
  ).toBeVisible();
  expect(await page.locator("#view").textContent()).toBe("");
  await page.getByLabel("账户", { exact: true }).fill("reader");
  await page
    .getByLabel("密码", { exact: true })
    .fill("browser-test-password-only");
  await page.getByRole("button", { name: "登录工作台" }).click();
  await page.getByRole("link", { name: "发送通道" }).click();
  await expect(
    page.getByRole("heading", { name: "发送通道", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "添加通道" })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "编辑", exact: true }),
  ).toHaveCount(0);
  // The browser cannot bypass backend authorization with a handcrafted write.
  const denied = await page.evaluate(async () => {
    const session = await fetch("/console/session").then((r) => r.json());
    return (
      await fetch("/console/api/providers", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": session.csrf,
        },
        body: "{}",
      })
    ).status;
  });
  expect(denied).toBe(403);
  await context.clearCookies();
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "登录 RelayTale" }),
  ).toBeVisible();
  expect(await page.locator("#view").textContent()).toBe("");
});
test("empty state, fetch failure, keyboard focus and client session expiry", async ({
  page,
}) => {
  await page.goto("/console/");
  await page.getByLabel("账户", { exact: true }).fill("owner");
  await page
    .getByLabel("密码", { exact: true })
    .fill("browser-test-password-only");
  await page.getByRole("button", { name: "登录工作台" }).click();
  await expect(
    page.getByRole("heading", { name: "邮件记录", exact: true }),
  ).toBeVisible();
  await page.route("**/console/api/messages?*", (route) =>
    route.fulfill({
      contentType: "application/json",
      body: '{"items":[],"next_after":"","has_more":false}',
    }),
  );
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "还没有邮件记录" }),
  ).toBeVisible();
  await page.unroute("**/console/api/messages?*");
  await page.route("**/console/api/messages?*", (route) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: '{"status":"operation_failed"}',
    }),
  );
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "未能加载记录" }),
  ).toBeVisible();
  await expect(page.getByRole("alert").first()).toContainText("未能确认");
  await page.unroute("**/console/api/messages?*");
  await page.getByRole("button", { name: "重新加载" }).click();
  await expect(
    page.getByRole("heading", { name: "邮件记录", exact: true }),
  ).toBeVisible();
  await page.getByRole("link", { name: "地址抑制" }).click();
  await page.getByRole("button", { name: "添加抑制" }).click();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).not.toBeVisible();
  // Server-side expiry has separate clock tests; this checks idle UI data removal.
  await page.clock.install();
  await page.getByRole("link", { name: "邮件记录" }).click();
  await expect(
    page.getByRole("heading", { name: "邮件记录", exact: true }),
  ).toBeVisible();
  await page.clock.fastForward(16 * 60 * 1000);
  await expect(
    page.getByRole("heading", { name: "登录 RelayTale" }),
  ).toBeVisible();
  expect(await page.locator("#view").textContent()).toBe("");
});
