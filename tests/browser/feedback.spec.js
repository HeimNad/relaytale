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

test("administrator publishes and withdraws a human feedback review", async ({
  page,
}) => {
  const linkID = "019a0000-0000-7000-8000-000000000001";
  let item;
  let linkedRequest;
  await page.route("**/console/api/feedback/*/links", async (route) => {
    if (route.request().method() === "GET")
      return route.fulfill({ json: { items: item ? [item] : [] } });
    linkedRequest = route.request().postDataJSON();
    item = {
      id: linkID,
      recipient_id: linkedRequest.recipient_id,
      attempt_id: linkedRequest.attempt_id,
      state: "PENDING_REVIEW",
      source_trust: "UNVERIFIED",
      revision: 0,
    };
    return route.fulfill({ status: 201, json: item });
  });
  await page.route("**/console/api/feedback-links/*/reviews", async (route) => {
    const body = route.request().postDataJSON();
    expect(body.acknowledge_unverified).toBe(true);
    expect(body.expected_revision).toBe(item.revision);
    item = {
      ...item,
      revision: item.revision + 1,
      state:
        body.action === "WITHDRAW"
          ? "WITHDRAWN"
          : "FAILURE_CONFIRMED_BY_OPERATOR",
    };
    return route.fulfill({ json: item });
  });
  await page.goto("/console/");
  await page.getByLabel("账户", { exact: true }).fill("owner");
  await page
    .getByLabel("密码", { exact: true })
    .fill("browser-test-password-only");
  await page.getByRole("button", { name: "登录工作台" }).click();
  await page.getByRole("link", { name: "反馈待查", exact: true }).click();
  await page.getByRole("link", { name: "格式异常样例" }).click();
  await page.getByText("新建人工关联", { exact: true }).click();
  for (const label of [
    "邮件 Gateway ID",
    "Provider ID",
    "投递尝试 ID",
    "收件人记录 ID",
  ])
    await page.getByRole("textbox", { name: label, exact: true }).fill(linkID);
  await page
    .getByRole("textbox", { name: "关联理由", exact: true })
    .fill("已核对原始邮件和收件人");
  await page.getByRole("checkbox", { name: /格式不标准/ }).check();
  await page
    .getByRole("checkbox", { name: "我知道关联由人工提供，来源未经验证" })
    .check();
  await page.getByRole("button", { name: "关联并通知网站待核验" }).click();
  await expect(page.getByText("待人工核验", { exact: true })).toBeVisible();
  expect(linkedRequest.manual_association).toBe(true);
  await page
    .getByRole("textbox", { name: "核验理由", exact: true })
    .fill("手工核对失败回执");
  await page.getByRole("checkbox", { name: /此操作会通知网站/ }).check();
  await page.getByRole("button", { name: "保存核验结果" }).click();
  await expect(
    page.getByText("人工确认失败 · 来源未验证", { exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: "test-results/manual-feedback-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "test-results/manual-feedback-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page
    .getByRole("textbox", { name: "核验理由", exact: true })
    .fill("关联错误，撤回结论");
  await page.getByRole("checkbox", { name: /此操作会通知网站/ }).check();
  await page.getByRole("button", { name: "保存核验结果" }).click();
  await expect(page.getByText("已撤回", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "保存核验结果" })).toHaveCount(
    0,
  );
});
