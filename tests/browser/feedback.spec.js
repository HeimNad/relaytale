const { test, expect } = require("@playwright/test");
test("viewer can inspect feedback without changing delivery", async ({
  page,
}) => {
  await page.goto("/console/");
  await page.getByLabel("账户", { exact: true }).fill("reader");
  await page
    .getByLabel("密码", { exact: true })
    .fill("browser-test-password-only");
  await page.getByRole("button", { name: "登录工作台" }).click();
  await page.getByRole("link", { name: "反馈待查", exact: true }).click();
  await page.getByRole("link", { name: "浏览器退信样例" }).click();
  await expect(page.getByRole("heading", { name: "反馈详情" })).toBeVisible();
  await expect(page.getByRole("note")).toContainText("自动处置关闭");
  await page.getByRole("button", { name: "查看格式说明" }).click();
  await expect(
    page.getByText("严格解析通过，来源仍未经验证。", { exact: true }),
  ).toBeVisible();
  await page
    .getByLabel("原始 Message-ID（人工提供）")
    .fill("<browser-feedback@example.test>");
  await page.getByRole("button", { name: "查找关联候选" }).click();
  await expect(
    page.getByText("2 个候选。候选仅供核对，不能证明反馈真实。"),
  ).toBeVisible();
  await expect(page.locator("#view")).toContainText("结果待确认");
  await page.screenshot({
    path: "test-results/feedback-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "test-results/feedback-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.getByRole("link", { name: "返回反馈列表" }).click();
  await page.getByRole("link", { name: "格式异常样例" }).click();
  await page.getByRole("button", { name: "查看格式说明" }).click();
  await expect(
    page.getByText("严格解析未通过；以下说明仅用于人工排查。", { exact: true }),
  ).toBeVisible();
  await expect(page.locator("#view img")).toHaveCount(0);
  await page.route("**/console/api/feedback/*/explanation?*", (route) =>
    route.fulfill({
      json: {
        strictly_parsed: false,
        issues: [],
        blocks: [
          {
            fields: {
              Diagnostic: [
                "<img src=https://untrusted.invalid/pixel onerror=window.injected=true>",
              ],
            },
          },
        ],
      },
    }),
  );
  await page.getByRole("button", { name: "查看格式说明" }).click();
  await expect(page.locator(".feedback-data").last()).toContainText(
    "<img src=",
  );
  await expect(page.locator("#view img")).toHaveCount(0);
  expect(await page.evaluate(() => window.injected)).toBeUndefined();
  let release;
  const held = new Promise((resolve) => {
    release = resolve;
  });
  await page.route("**/console/api/feedback/*/candidates?*", async (route) => {
    await held;
    await route.fulfill({
      json: {
        candidates: [],
        reasons: [],
        multiple_messages: false,
        truncated: false,
      },
    });
  });
  await page
    .getByLabel("原始 Message-ID（人工提供）")
    .fill("<absent@example.test>");
  const pending = page.waitForRequest("**/console/api/feedback/*/candidates?*");
  await page.getByRole("button", { name: "查找关联候选" }).click();
  await pending;
  await page.getByRole("link", { name: "返回反馈列表" }).click();
  release();
  await expect(
    page.getByRole("heading", { name: "反馈待查", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("0 个候选。候选仅供核对，不能证明反馈真实。"),
  ).toHaveCount(0);
});
