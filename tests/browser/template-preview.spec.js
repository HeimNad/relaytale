const { test, expect } = require("@playwright/test");
test("template preview: sandbox, no network, sample reset and stale revision", async ({
  page,
}) => {
  const external = [];
  page.on("request", (r) => {
    if (r.url().includes("untrusted.invalid")) external.push(r.url());
  });
  await page.goto("/console/");
  await page.getByLabel("账户", { exact: true }).fill("owner");
  await page
    .getByLabel("密码", { exact: true })
    .fill("browser-test-password-only");
  await page.getByRole("button", { name: "登录工作台" }).click();
  await page.getByRole("link", { name: "邮件模板", exact: true }).click();
  await page
    .getByLabel("网站身份", { exact: true })
    .selectOption({ label: "模板验收网站 / 生产" });
  await page
    .getByRole("row")
    .filter({ hasText: "welcome" })
    .getByRole("button", { name: "预览内容" })
    .click();
  await expect(
    page.getByRole("button", { name: "审阅并发布这个修订" }),
  ).toBeDisabled();
  await page
    .getByLabel("name", { exact: true })
    .fill("<script>测试客户</script>");
  await page.getByRole("button", { name: "生成预览" }).click();
  await expect(page.getByRole("heading", { name: "渲染结果" })).toBeVisible();
  const frame = page.frameLocator('iframe[title="隔离 HTML 预览"]');
  await expect(frame.locator("body")).toContainText(
    "你好 <script>测试客户</script>",
  );
  await expect(frame.locator("body")).toContainText("[图片已隐藏]");
  await expect(frame.locator("script,img,a,style,iframe,form,svg")).toHaveCount(
    0,
  );
  expect(await page.locator("iframe").getAttribute("sandbox")).toBe("");
  expect(await page.evaluate(() => window.injected)).toBeUndefined();
  await page.screenshot({
    path: "test-results/preview-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.locator("iframe").scrollIntoViewIfNeeded();
  await expect(frame.locator("body")).toContainText(
    "你好 <script>测试客户</script>",
  );
  await page.screenshot({ path: "test-results/preview-mobile.png" });
  await page.getByLabel("name", { exact: true }).fill("新的样例");
  await expect(page.locator("iframe")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "审阅并发布这个修订" }),
  ).toBeDisabled();
  const status = await page.evaluate(async () => {
    const session = await fetch("/console/session").then((r) => r.json());
    const identities = await fetch("/console/api/template-identities").then(
      (r) => r.json(),
    );
    const identity = identities.items.find((i) => i.name === "模板验收网站");
    return (
      await fetch(`/console/api/templates/${identity.id}/drafts`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": session.csrf,
        },
        body: JSON.stringify({
          expected_revision: 1,
          template: {
            id: "welcome",
            subject: "新主题 {{name}}",
            text: "新内容 {{name}}",
            variables: ["name"],
          },
          reason: "concurrent preview test",
        }),
      })
    ).status;
  });
  expect(status).toBe(200);
  await page.getByRole("button", { name: "生成预览" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "草稿已被修改" }),
  ).toBeVisible();
  await expect(page.locator("iframe")).toHaveCount(0);
  expect(external).toEqual([]);
});
