const { test, expect } = require("@playwright/test");
test("templates: inline edit, conflict retention, guarded publication, retirement and roles", async ({
  page,
}) => {
  const external = [];
  page.on("request", (r) => {
    if (r.url().includes("untrusted.invalid")) external.push(r.url());
  });
  const login = async (user) => {
    await page.goto("/console/");
    await page.getByLabel("账户", { exact: true }).fill(user);
    await page
      .getByLabel("密码", { exact: true })
      .fill("browser-test-password-only");
    await page.getByRole("button", { name: "登录工作台" }).click();
    await expect(
      page.getByRole("heading", { name: "邮件记录", exact: true }),
    ).toBeVisible();
    await page.getByRole("link", { name: "邮件模板", exact: true }).click();
    await page
      .getByLabel("网站身份", { exact: true })
      .selectOption({ label: "模板验收网站 / 生产" });
    await expect(
      page.getByRole("row").filter({ hasText: "welcome" }),
    ).toBeVisible();
  };
  await login("owner");
  await page
    .getByRole("row")
    .filter({ hasText: "welcome" })
    .getByRole("button", { name: "审阅并发布" })
    .click();
  await expect(page.locator(".template-source").last()).toContainText(
    "https://untrusted.invalid/template-pixel",
  );
  await page.getByRole("button", { name: "取消", exact: true }).click();
  await page.screenshot({
    path: "test-results/templates-desktop.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "新建草稿", exact: true }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page.getByLabel("模板名称", { exact: true }).fill("receipt");
  await page
    .getByLabel("邮件主题（可选）", { exact: true })
    .fill("订单确认 {{name}}");
  await page.getByLabel("纯文本正文", { exact: true }).fill("您好 {{name}}");
  await page.getByLabel("变量名称（逗号分隔）", { exact: true }).fill("name");
  await page
    .getByLabel("处理理由", { exact: true })
    .fill("browser draft creation");
  await page.screenshot({
    path: "test-results/template-editor-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "test-results/template-editor-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.getByRole("button", { name: "保存草稿", exact: true }).click();
  await expect(
    page.getByRole("row").filter({ hasText: "receipt" }),
  ).toBeVisible();
  await page
    .getByRole("row")
    .filter({ hasText: "receipt" })
    .getByRole("button", { name: "编辑草稿" })
    .click();
  await page
    .getByLabel("纯文本正文", { exact: true })
    .fill("我的编辑 {{name}}");
  await page.getByLabel("处理理由", { exact: true }).fill("browser stale edit");
  // The editor keeps the selected identity in application state; use fixture metadata API for the competing edit.
  const concurrent = await page.evaluate(async () => {
    const session = await (await fetch("/console/session")).json();
    const identities = await (
      await fetch("/console/api/template-identities")
    ).json();
    const id = identities.items.find((k) => k.name === "模板验收网站").id;
    return (
      await fetch(`/console/api/templates/${id}/drafts`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": session.csrf,
        },
        body: JSON.stringify({
          expected_revision: 1,
          template: {
            id: "receipt",
            subject: "订单确认 {{name}}",
            text: "另一份编辑 {{name}}",
            html: "",
            variables: ["name"],
          },
          reason: "concurrent browser edit",
        }),
      })
    ).status;
  });
  expect(concurrent).toBe(200);
  await page.getByRole("button", { name: "保存草稿", exact: true }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "草稿已被修改" }),
  ).toBeVisible();
  await expect(page.getByLabel("纯文本正文", { exact: true })).toHaveValue(
    "我的编辑 {{name}}",
  );
  await page.getByRole("button", { name: "返回模板目录" }).click();
  await page
    .getByRole("row")
    .filter({ hasText: "receipt" })
    .getByRole("button", { name: "审阅并发布" })
    .click();
  await page
    .getByLabel("处理理由", { exact: true })
    .fill("browser publication");
  await page.getByRole("button", { name: "发布此修订" }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.getByLabel("我已核对这个修订的内容与变量。").check();
  await page.getByRole("button", { name: "发布此修订" }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await expect(page.getByRole("alert")).toContainText("模板版本已发布");
  await page.getByRole("button", { name: "退役版本" }).click();
  await page.getByLabel("我已确认业务网站的请求重试窗口结束。").check();
  await page.getByLabel("处理理由", { exact: true }).fill("browser retirement");
  await page.getByRole("button", { name: "记录退役" }).click();
  await expect(page.getByText("已记录退役", { exact: true })).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "test-results/templates-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.getByRole("button", { name: "退出登录" }).click();
  await login("editor");
  await expect(
    page.getByRole("button", { name: "新建草稿", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "审阅并发布" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "退役版本" })).toHaveCount(0);
  await page.getByRole("button", { name: "退出登录" }).click();
  await login("reader");
  await expect(
    page.getByRole("button", { name: "新建草稿", exact: true }),
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "编辑草稿" })).toHaveCount(0);
  expect(await page.locator("#view").textContent()).not.toMatch(/\bnull\b/);
  expect(external).toEqual([]);
  expect(await page.evaluate(() => window.injected)).toBeUndefined();
});
