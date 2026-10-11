const { test, expect } = require("@playwright/test");

test("real admin review reaches website through signed durable callbacks", async ({
  page,
}) => {
  await page.goto("/console/");
  await page.getByLabel("账户", { exact: true }).fill("owner");
  await page
    .getByLabel("密码", { exact: true })
    .fill("browser-test-password-only");
  await page.getByRole("button", { name: "登录工作台" }).click();
  await page.getByRole("link", { name: "反馈待查", exact: true }).click();
  await page.getByRole("link", { name: "网站全链路退信样例" }).click();
  await page
    .getByLabel("原始 Message-ID（人工提供）")
    .fill("<website-browser@example.test>");
  await page.getByRole("button", { name: "查找关联候选" }).click();
  await page.getByRole("button", { name: "选择此候选" }).click();
  await page
    .getByRole("textbox", { name: "关联理由", exact: true })
    .fill("已核对受控测试邮件及收件人");
  await page
    .getByRole("checkbox", { name: "我知道关联由人工提供，来源未经验证" })
    .check();
  await page.getByRole("button", { name: "关联并通知网站待核验" }).click();
  const websiteState = async (state) => {
    await expect
      .poll(
        async () => {
          const response = await page.request.get("/e2e/website");
          expect(response.status()).toBe(200);
          const record = await response.json();
          if (!record.snapshot.message_id) return "pending";
          expect(JSON.stringify(record.snapshot)).toContain("SMTP_ACCEPTED");
          if (record.needs_refresh) return "pending";
          const items = record.feedback.items || [];
          if (!items.length) return "pending";
          expect(items[0].source_trust).toBe("UNVERIFIED");
          expect(record.feedback.automatic_suppression).toBe(false);
          return items[0].state;
        },
        { timeout: 15000 },
      )
      .toBe(state);
  };
  await websiteState("PENDING_REVIEW");
  await page
    .getByRole("textbox", { name: "核验理由", exact: true })
    .fill("管理员核对测试失败报告");
  await page.getByRole("checkbox", { name: /此操作会通知网站/ }).check();
  await page.getByRole("button", { name: "保存核验结果" }).click();
  await expect(
    page.getByText("人工确认失败 · 来源未验证", { exact: true }),
  ).toBeVisible();
  await websiteState("FAILURE_CONFIRMED_BY_OPERATOR");
  await page
    .getByRole("textbox", { name: "核验理由", exact: true })
    .fill("撤回测试核验结论");
  await page.getByRole("checkbox", { name: /此操作会通知网站/ }).check();
  await page.getByRole("button", { name: "保存核验结果" }).click();
  await expect(page.getByText("已撤回", { exact: true })).toBeVisible();
  await websiteState("WITHDRAWN");
});
