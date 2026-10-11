"use strict";
const $ = (s) => document.querySelector(s);
const S = {
  user: null,
  csrf: "",
  view: "messages",
  generation: 0,
  cursors: [],
  after: "",
  page: null,
};
const titles = {
  statistics: "统计与容量",
  messages: "邮件记录",
  providers: "发送通道",
  suppressions: "地址抑制",
  feedback: "反馈待查",
  access: "访问与说明",
  templates: "邮件模板",
};
const roles = { admin: "管理员", operator: "操作员", viewer: "只读观察员" };
const states = {
  IN_PROGRESS: ["正在投递", "accepted"],
  QUEUED: ["等待投递", ""],
  SENDING: ["正在投递", "accepted"],
  SMTP_ACCEPTED: ["SMTP 已接受", "accepted"],
  PARTIAL_ACCEPTED: ["部分已接受", "warn"],
  TEMP_FAILED: ["暂时失败", "warn"],
  PERM_FAILED: ["永久失败", "bad"],
  DELIVERY_UNKNOWN: ["结果待确认", "warn"],
  DELIVERED: ["已确认收到", "good"],
  BOUNCED: ["已退信", "bad"],
  SUPPRESSED: ["已抑制", ""],
  CANCELLED: ["已取消", ""],
  EXPIRED: ["已过期", ""],
  SANDBOX_STORED: ["测试存档 · 不发信", ""],
  RECEIVED: ["已接收", ""],
  ARCHIVED: ["已存档", ""],
  HEALTHY: ["健康", "good"],
  DEGRADED: ["降级", "warn"],
  UNHEALTHY: ["不可用", "bad"],
  DISABLED: ["已停用", ""],
  CLOSED: ["熔断关闭", ""],
  OPEN: ["熔断中", "warn"],
  HALF_OPEN: ["半开探测", "warn"],
};
const eventNames = {
  QUOTA_RESERVED: "预留投递额度",
  DNS_RESOLVED: "地址解析完成",
  SMTP_GREETING: "收到 SMTP 问候",
  EHLO_COMPLETED: "确认服务能力",
  AUTH_STARTED: "开始身份认证",
  QUIT_SENT: "结束 SMTP 会话",
  RCPT_REJECTED: "收件人被拒绝",
  DELIVERY_DECIDED: "记录收件人投递决策",
  DELIVERY_UNKNOWN: "投递结果待人工确认",

  MESSAGE_RECEIVED: "网关已接收",
  MESSAGE_ARCHIVED: "原文已存档",
  MESSAGE_QUEUED: "进入投递队列",
  ATTEMPT_STARTED: "开始投递尝试",
  DNS_STARTED: "开始解析地址",
  DNS_COMPLETED: "地址解析完成",
  TCP_CONNECTED: "建立连接",
  TLS_ESTABLISHED: "TLS 已建立",
  AUTH_SUCCEEDED: "认证通过",
  MAIL_FROM_ACCEPTED: "发件人已接受",
  RCPT_ACCEPTED: "收件人已接受",
  DATA_ARMED: "已授权传输正文",
  DATA_STARTED: "开始传输正文",
  DATA_COMPLETED: "正文传输完成",
  SMTP_ACCEPTED: "SMTP 已接受",
  ATTEMPT_FAILED: "投递尝试失败",
  UNKNOWN_RESOLVED: "人工处置已记录",
  RECIPIENT_SUPPRESSED: "收件地址已抑制",
  RETRY_QUEUED: "重试已入队",
};
function el(tag, props = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v == null) continue;
    if (k.startsWith("on")) n.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k === "value") n.value = v;
    else if (k === "checked") n.checked = v;
    else n.setAttribute(k, v);
  }
  for (const c of children.flat(Infinity)) {
    if (c != null)
      n.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return n;
}
const button = (text, fn, cls = "") =>
  el("button", { type: "button", class: cls, onClick: fn }, text);
const badge = (status) => {
  const v = states[status] || [status || "尚无观测", ""];
  return el("span", { class: "badge " + v[1] }, v[0]);
};
const date = (s) =>
  s ? new Date(s).toLocaleString("zh-CN", { hour12: false }) : "—";
const canWrite = () => S.user?.role !== "viewer";
const canAdmin = () => S.user?.role === "admin";
function problem(message, tone = "") {
  $("#notice").hidden = !message;
  $("#notice").className = "notice" + (tone ? " " + tone : "");
  $("#notice").textContent = message || "";
}
function errorText(e) {
  if (S.view === "templates" && e.code === "draft_revision_conflict")
    return "草稿已被修改。你的输入仍保留，请取消后重新读取当前修订并核对，再保存或发布。";
  if (S.view === "templates" && e.code === "invalid_template_preview")
    return "无法生成预览。请完整填写声明的变量，核对语言、修订和渲染后的大小。";
  if (S.view === "templates" && e.code === "invalid_template_or_limits")
    return "模板内容或容量不符合要求。核对变量名称、正文和字节限制，再保存。";
  if (S.view === "statistics" && e.status === 400)
    return "统计时间范围无效，请核对本机时间。";
  if (S.view === "statistics" && e.status === 422)
    return "统计范围过大，请缩短查询时间。未展示不完整的汇总。";
  return (
    {
      401: "会话已失效，请重新登录。",
      403: "没有此操作的权限，或请求来源验证未通过。",
      409: "对象已变化或不满足处理条件。请刷新后重新核对，不要重复提交。",
      400: "请检查输入内容和必填字段。",
      422: "请求不满足处理条件，请核对输入和对象状态。",
      415: "请求格式不受支持。",
      429: "登录尝试过于频繁，请稍后再试。",
      503: "操作未能确认完成。请刷新核对对象状态，再决定是否继续。",
    }[e.status] || "连接中断，未能确认结果。请刷新核对状态后再继续。"
  );
}
async function request(path, options = {}) {
  const method = options.method || "GET";
  const headers = {};
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = S.csrf;
  let r;
  try {
    r = await fetch("/console/" + path, {
      method,
      headers,
      credentials: "same-origin",
      cache: "no-store",
      body:
        options.body === undefined ? undefined : JSON.stringify(options.body),
    });
  } catch {
    throw new Error("connection");
  }
  if (!r.ok) {
    const e = new Error("request failed");
    e.status = r.status;
    try {
      e.code = (await r.json()).status;
    } catch {}
    if (r.status === 401 && path !== "login" && path !== "session") {
      signedOut();
    }
    throw e;
  }
  const payload = await r.json();
  if (S.user) armSession();
  return payload;
}
const api = (path, body, method) =>
  request("api/" + path, {
    body,
    method: method || (body === undefined ? "GET" : "POST"),
  });
function signedOut() {
  clearTimeout(S.timer);
  S.generation++;
  S.user = null;
  S.csrf = "";
  S.page = null;
  S.cursors = [];
  S.after = "";
  $("#view").replaceChildren();
  $("#notice").textContent = "";
  $("#modal").close();
  $("#modal").replaceChildren();
  $("#shell").hidden = true;
  $("#login").hidden = false;
  $("#boot").hidden = true;
  $("#login-form").reset();
}
function armSession() {
  clearTimeout(S.timer);
  const remaining = Math.min(
    15 * 60 * 1000,
    new Date(S.user.expires_at).getTime() - Date.now(),
  );
  S.timer = setTimeout(
    () => {
      signedOut();
      $("#login-error").textContent = "会话已到期，请重新登录。";
    },
    Math.max(0, remaining),
  );
}
function signedIn(user) {
  S.user = user;
  armSession();
  S.csrf = user.csrf;
  $("#identity").textContent = user.id;
  $("#avatar").textContent = user.id[0].toUpperCase();
  $("#role").textContent = roles[user.role];
  $("#login").hidden = true;
  $("#shell").hidden = false;
  $("#boot").hidden = true;
  route();
}
$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.currentTarget;
  const b = form.querySelector("button");
  b.disabled = true;
  $("#login-error").textContent = "";
  const data = Object.fromEntries(new FormData(form));
  try {
    const user = await request("login", { method: "POST", body: data });
    form.reset();
    signedIn(user);
  } catch (err) {
    form.elements.password.value = "";
    $("#login-error").textContent =
      err.status === 401 ? "账户或密码不正确。" : errorText(err);
  } finally {
    data.password = "";
    b.disabled = false;
  }
});
async function logout() {
  try {
    await request("logout", { method: "POST", body: {} });
    signedOut();
  } catch (e) {
    problem(errorText(e));
  }
}
$("#logout").addEventListener("click", logout);
$("#logout-mobile").addEventListener("click", logout);
function heading(title, description, ...actions) {
  return el(
    "div",
    { class: "page-heading" },
    el("div", {}, el("h1", {}, title), el("p", {}, description)),
    el("div", { class: "actions" }, actions),
  );
}
function empty(title, description) {
  return el(
    "div",
    { class: "empty" },
    el("div", { class: "empty-symbol", "aria-hidden": "true" }, "✉"),
    el("h2", {}, title),
    el("p", {}, description),
  );
}
function facts(items) {
  return el(
    "dl",
    { class: "facts" },
    Object.entries(items).flatMap(([k, v]) => [
      el("dt", {}, k),
      el("dd", {}, v ?? "—"),
    ]),
  );
}
function pager(data, reload) {
  return el(
    "div",
    { class: "pager" },
    el(
      "p",
      {},
      `当前页 ${data.items.length} 条${data.has_more ? "，还有更多记录" : ""}`,
    ),
    el(
      "div",
      { class: "actions" },
      S.cursors.length
        ? button("上一页", () => {
            S.after = S.cursors.pop();
            reload();
          })
        : null,
      data.has_more
        ? button("下一页", () => {
            S.cursors.push(S.after);
            S.after = data.next_after;
            reload();
          })
        : null,
    ),
  );
}
function table(headers, rows) {
  for (const row of rows)
    Array.from(row.children).forEach((td, i) =>
      td.setAttribute("data-label", headers[i]),
    );
  return el(
    "div",
    { class: "table-wrap" },
    el(
      "table",
      {},
      el(
        "thead",
        {},
        el(
          "tr",
          {},
          headers.map((x) => el("th", { scope: "col" }, x)),
        ),
      ),
      el("tbody", {}, rows),
    ),
  );
}
function dialog(title, content, label, save, description = "") {
  const d = $("#modal");
  d.replaceChildren();
  let error = el("p", { class: "form-error", role: "alert" });
  const submit = el("button", { class: "primary", type: "submit" }, label);
  const cancel = button("取消", () => d.close());
  const form = el(
    "form",
    {
      onSubmit: async (e) => {
        e.preventDefault();
        if (!form.reportValidity()) return;
        submit.disabled = true;
        cancel.disabled = true;
        error.textContent = "";
        try {
          const result = await save(new FormData(form));
          d.close();
          d.replaceChildren();
          problem("");
          route();
          if (result?.notice) problem(result.notice, "success");
        } catch (err) {
          error.textContent = errorText(err);
        } finally {
          submit.disabled = false;
          cancel.disabled = false;
        }
      },
    },
    el("h2", { id: "dialog-title" }, title),
    description ? el("p", {}, description) : null,
    content,
    error,
    el("div", { class: "dialog-actions" }, cancel, submit),
  );
  d.append(form);
  d.showModal();
}
function field(label, name, value = "", type = "text", extra = {}) {
  return el(
    "label",
    { class: extra.wide ? "wide" : null },
    label,
    el(type === "textarea" ? "textarea" : "input", {
      name,
      value,
      type: type === "textarea" ? null : type,
      required: extra.optional ? null : "",
      ...extra,
    }),
  );
}
function reason() {
  return field("处理理由", "reason", "", "textarea", {
    maxlength: 2048,
    wide: true,
  });
}
function summary(name, id) {
  return el("div", { class: "object-summary" }, name, el("code", {}, id));
}
function providerForm(p = null) {
  const s = p || {
    name: "",
    host: "",
    port: 587,
    security: "starttls",
    username: "",
    priority: 10,
    max_connections: 1,
    timeout_seconds: 30,
    hourly_limit: 0,
    daily_limit: 0,
    from_domains: [],
    enabled: false,
  };
  const fields = el(
    "div",
    { class: "form-grid" },
    field("通道名称", "name", s.name, "text", { maxlength: 128 }),
    field("SMTP 主机", "host", s.host, "text", { maxlength: 253 }),
    field("端口", "port", s.port, "number", { min: 1, max: 65535 }),
    el(
      "label",
      {},
      "加密方式",
      el(
        "select",
        { name: "security" },
        ["starttls", "implicit_tls"].map((v) =>
          el(
            "option",
            { value: v, selected: v === s.security ? "" : null },
            v === "starttls" ? "STARTTLS" : "隐式 TLS",
          ),
        ),
      ),
    ),
    field("SMTP 用户名", "username", s.username, "text", { maxlength: 320 }),
    !p
      ? field("SMTP 密码", "password", "", "password", {
          autocomplete: "new-password",
          maxlength: 4096,
        })
      : null,
    field(
      "允许发件域（逗号分隔）",
      "from_domains",
      s.from_domains.join(", "),
      "text",
      { wide: true },
    ),
    field("优先级（越小越优先）", "priority", s.priority, "number", {
      min: -2147483648,
      max: 2147483647,
    }),
    field("并发连接", "max_connections", s.max_connections, "number", {
      min: 1,
      max: 32,
    }),
    field("超时秒数", "timeout_seconds", s.timeout_seconds, "number", {
      min: 1,
      max: 300,
    }),
    field("每小时额度（0 不限）", "hourly_limit", s.hourly_limit, "number", {
      min: 0,
      max: 2147483647,
    }),
    field("每日额度（0 不限）", "daily_limit", s.daily_limit, "number", {
      min: 0,
      max: 2147483647,
    }),
  );
  const content = el(
    "div",
    {},
    p ? summary(p.name, `配置版本 ${p.revision} / ${p.id}`) : null,
    fields,
    el(
      "label",
      { class: "check-label" },
      el("input", { type: "checkbox", name: "enabled", checked: s.enabled }),
      "启用通道，允许后续领取使用此配置",
    ),
    reason(),
  );
  const id = p?.id || crypto.randomUUID();
  dialog(
    p ? "编辑发送通道" : "添加发送通道",
    content,
    "保存配置",
    async (data) => {
      const settings = {};
      for (const k of ["name", "host", "security", "username"])
        settings[k] = data.get(k);
      for (const k of [
        "port",
        "priority",
        "max_connections",
        "timeout_seconds",
        "hourly_limit",
        "daily_limit",
      ])
        settings[k] = Number(data.get(k));
      settings.from_domains = data
        .get("from_domains")
        .split(",")
        .map((x) => x.trim());
      settings.enabled = data.has("enabled");
      const payload = { settings, reason: data.get("reason") };
      if (p) {
        payload.expected_revision = p.revision;
        await api("providers/" + id, payload, "PUT");
      } else {
        payload.id = id;
        payload.password = data.get("password");
        await api("providers", payload);
      }
    },
    "只影响后续领取。在途尝试不会被撤销；配置冲突时请刷新核对。",
  );
}
function rotate(p) {
  dialog(
    "轮换 SMTP 密码",
    el(
      "div",
      {},
      summary(p.name, `配置版本 ${p.revision} / ${p.id}`),
      field("新密码", "password", "", "password", {
        autocomplete: "new-password",
        maxlength: 4096,
      }),
      reason(),
    ),
    "确认轮换",
    (data) =>
      api("providers/" + p.id + "/credentials", {
        expected_revision: p.revision,
        password: data.get("password"),
        reason: data.get("reason"),
      }),
    "新密码不会回显；在途尝试仍使用原凭证快照。",
  );
}
function addSuppression() {
  dialog(
    "添加地址抑制",
    el(
      "div",
      { class: "form-grid" },
      field("邮箱地址", "email", "", "email", { maxlength: 320, wide: true }),
      el(
        "label",
        {},
        "分类",
        el(
          "select",
          { name: "category" },
          [
            ["manual", "人工抑制"],
            ["hard_bounce", "可信硬退信"],
            ["complaint", "投诉"],
            ["invalid", "无效地址"],
            ["unsubscribe", "退订"],
          ].map(([v, t]) => el("option", { value: v }, t)),
        ),
      ),
      field("到期时间（可留空）", "expires_at", "", "datetime-local", {
        optional: true,
      }),
      reason(),
    ),
    "确认添加",
    (data) =>
      api("suppressions", {
        email: data.get("email"),
        category: data.get("category"),
        expires_at: data.get("expires_at")
          ? new Date(data.get("expires_at")).toISOString()
          : null,
        reason: data.get("reason"),
      }),
    "将阻止该地址的新投递与待发送操作。已经获得正文传输许可的尝试无法撤回。",
  );
}
function release(s) {
  dialog(
    "解除地址抑制",
    el("div", {}, summary(s.email, s.id), reason()),
    "确认解除",
    (data) =>
      api("suppressions/" + s.id + "/release", { reason: data.get("reason") }),
    "解除后允许未来投递，不会自动重发历史邮件。",
  );
}
function resolveRecipient(r) {
  const select = el(
    "select",
    { name: "action" },
    el("option", { value: "mark-failed" }, "记录为永久失败"),
    el("option", { value: "mark-delivered" }, "记录为已确认收到"),
    el("option", { value: "retry" }, "重新排队投递"),
  );
  const acknowledge = el("input", { name: "acknowledge", type: "checkbox" });
  const risk = el(
    "label",
    { class: "check-label" },
    acknowledge,
    "我已核对证据，接受重新投递可能导致重复收信的风险。",
  );
  risk.hidden = true;
  select.addEventListener("change", () => {
    risk.hidden = select.value !== "retry";
    acknowledge.required = select.value === "retry";
    acknowledge.checked = false;
  });
  dialog(
    "处置待确认的投递",
    el(
      "div",
      {},
      summary(r.address, `尝试 ${r.latest_attempt_id}`),
      el(
        "p",
        { class: "callout warn" },
        "远端可能已接收这封邮件。此操作记录人工判断，不会改写原 SMTP 尝试。",
      ),
      el("label", {}, "处理方式", select),
      risk,
      reason(),
    ),
    "确认并记录",
    (data) =>
      api("recipients/" + r.id + "/resolve-unknown", {
        expected_attempt: r.latest_attempt_id,
        action: data.get("action"),
        reason: data.get("reason"),
        acknowledge_duplicate_risk: data.has("acknowledge"),
      }),
  );
}
async function listPage(view, generation) {
  const path =
    view === "messages"
      ? "messages"
      : view === "providers"
        ? "providers"
        : "suppressions";
  const data = await api(
    path +
      "?limit=50" +
      (S.after ? "&after=" + encodeURIComponent(S.after) : ""),
  );
  if (S.generation !== generation) return;
  S.page = data;
  const host = $("#view");
  host.replaceChildren();
  const refresh = () => {
    S.after = "";
    S.cursors = [];
    route(false);
  };
  host.append(
    heading(
      titles[view],
      view === "messages"
        ? "从接收到确认，查看每封邮件的投递事实。"
        : view === "providers"
          ? "管理投递通道，查看健康观测与预留用量。"
          : "保护收件人信誉，保留每一次添加和解除的记录。",
      button("刷新", refresh),
      view === "providers" && canAdmin()
        ? button("添加通道", () => providerForm(), "primary")
        : view === "suppressions" && canWrite()
          ? button("添加抑制", addSuppression, "primary")
          : null,
    ),
  );
  if (view === "providers") {
    const grid = el("div", { class: "provider-grid" });
    for (const p of data.items) {
      grid.append(
        el(
          "article",
          { class: "panel provider-card" },
          el("h2", {}, p.name),
          el("p", { class: "host" }, `${p.host}:${p.port}`),
          el(
            "div",
            { class: "badges" },
            badge(p.enabled ? p.health_status || "尚无观测" : "DISABLED"),
            p.circuit_state ? badge(p.circuit_state) : null,
          ),
          el(
            "div",
            { class: "usage" },
            el(
              "div",
              {},
              el("strong", {}, p.hourly_reserved),
              el("small", {}, `小时预留 / ${p.hourly_limit || "不限"}`),
            ),
            el(
              "div",
              {},
              el("strong", {}, p.daily_reserved),
              el("small", {}, `日预留 / ${p.daily_limit || "不限"}`),
            ),
          ),
          el("p", { class: "host" }, "最近成功：" + date(p.last_success_at)),
          el("p", { class: "host" }, "熔断恢复：" + date(p.open_until)),
          el(
            "div",
            { class: "actions" },
            button("查看配置", () => providerDetail(p)),
            canAdmin() ? button("编辑", () => providerForm(p)) : null,
            canAdmin() ? button("轮换密码", () => rotate(p)) : null,
          ),
        ),
      );
    }
    host.append(
      grid.childNodes.length
        ? grid
        : el(
            "div",
            { class: "panel" },
            empty(
              "尚未配置发送通道",
              "添加 SMTP Provider 后，符合路由资格的邮件才能通过该通道发送。",
            ),
          ),
      pager(data, () => route(false)),
    );
    return;
  }
  const panel = el("div", { class: "panel" });
  const input = el("input", {
    type: "search",
    placeholder:
      view === "messages" ? "筛选当前页的主题或发件人" : "筛选当前页的地址",
    "aria-label": "筛选当前页",
  });
  const select = el(
    "select",
    { "aria-label": "筛选状态" },
    el("option", { value: "" }, "所有状态"),
    ...(view === "messages"
      ? Object.keys(states)
          .filter((k) =>
            [
              "QUEUED",
              "SENDING",
              "SMTP_ACCEPTED",
              "PARTIAL_ACCEPTED",
              "TEMP_FAILED",
              "PERM_FAILED",
              "DELIVERY_UNKNOWN",
              "DELIVERED",
              "BOUNCED",
              "SUPPRESSED",
              "CANCELLED",
              "EXPIRED",
              "SANDBOX_STORED",
            ].includes(k),
          )
          .map((k) => el("option", { value: k }, states[k][0]))
      : [
          el("option", { value: "active" }, "生效中"),
          el("option", { value: "inactive" }, "历史记录"),
        ]),
  );
  const contents = el("div");
  function renderRows() {
    const q = input.value.toLowerCase();
    const items = data.items.filter(
      (x) =>
        JSON.stringify(
          view === "messages" ? [x.subject, x.envelope_from, x.id] : [x.email],
        )
          .toLowerCase()
          .includes(q) &&
        (!select.value ||
          (view === "messages"
            ? x.status === select.value
            : (select.value === "active") === x.active)),
    );
    const rows = items.map((x) =>
      view === "messages"
        ? el(
            "tr",
            {},
            el("td", {}, badge(x.status)),
            el(
              "td",
              {},
              button(
                x.subject || "（无主题）",
                () => {
                  location.hash = "messages/" + x.id;
                },
                "subject",
              ),
              el("small", {}, x.envelope_from),
            ),
            el("td", {}, date(x.created_at)),
            el("td", {}, x.attempt_count),
          )
        : el(
            "tr",
            {},
            el("td", {}, el("strong", {}, x.email), el("small", {}, x.note)),
            el("td", {}, badge(x.active ? "生效中" : "历史记录")),
            el("td", {}, x.category, el("small", {}, x.actor)),
            el("td", {}, date(x.expires_at)),
            el(
              "td",
              {},
              canWrite() && x.active ? button("解除", () => release(x)) : null,
            ),
          ),
    );
    contents.replaceChildren(
      rows.length
        ? table(
            view === "messages"
              ? ["投递状态", "邮件 / 发件人", "接收时间", "尝试"]
              : [
                  "地址 / 处理理由",
                  "状态",
                  "分类 / 操作者",
                  "到期时间",
                  "操作",
                ],
            rows,
          )
        : empty(
            data.items.length
              ? "没有匹配的记录"
              : view === "messages"
                ? "还没有邮件记录"
                : "尚无抑制记录",
            data.items.length
              ? "调整当前页筛选，或翻页查看更多记录。"
              : view === "messages"
                ? "通过已配置的 SMTP 账户提交邮件后，记录会显示在这里。"
                : "需要暂停向某个地址投递时，添加抑制并留下处理理由。",
          ),
    );
  }
  input.addEventListener("input", renderRows);
  select.addEventListener("change", renderRows);
  panel.append(
    el(
      "div",
      { class: "toolbar" },
      input,
      select,
      el("small", {}, "筛选仅作用于当前页"),
    ),
    contents,
    pager(data, () => route(false)),
  );
  host.append(panel);
  renderRows();
}
function providerDetail(p) {
  const content = el(
    "div",
    {},
    facts({
      通道: p.name,
      配置版本: p.revision,
      主机: p.host,
      "端口 / 加密": `${p.port} / ${p.security}`,
      用户名: p.username,
      允许发件域: p.from_domains.join(", "),
      优先级: p.priority,
      最大连接: p.max_connections,
      超时: p.timeout_seconds + " 秒",
      最近失败: date(p.last_failure_at),
    }),
  );
  const d = $("#modal");
  d.replaceChildren(
    el(
      "form",
      { method: "dialog" },
      el("h2", { id: "dialog-title" }, "通道配置"),
      content,
      el(
        "div",
        { class: "dialog-actions" },
        button("关闭", () => d.close()),
      ),
    ),
  );
  d.showModal();
}
async function messageDetail(id, generation) {
  const [m, rs, ats, evs] = await Promise.all([
    api("messages/" + id),
    api("messages/" + id + "/recipients?limit=100"),
    api("messages/" + id + "/attempts?limit=100"),
    api("messages/" + id + "/events?limit=100"),
  ]);
  if (S.generation !== generation) return;
  const host = $("#view");
  host.replaceChildren(
    heading(
      "投递详情",
      "从持久化记录还原过程，不推断收件结果。",
      button("返回邮件", () => {
        location.hash = "messages";
      }),
      button("刷新", () => route(false)),
    ),
  );
  const recipients = el(
    "section",
    { class: "detail-section" },
    el("h2", {}, "收件人"),
  );
  const timeline = el("ol", {
    class: "timeline",
    tabindex: "0",
    "aria-label": "投递事件记录",
  });
  const attempts = el(
    "section",
    { class: "detail-section" },
    el("h2", {}, "尝试记录"),
  );
  const addRecipients = (items) => {
    for (const r of items)
      recipients.append(
        el(
          "article",
          { class: "recipient" },
          el(
            "div",
            { class: "recipient-title" },
            el("strong", {}, r.address),
            badge(r.status),
          ),
          el(
            "small",
            { class: "muted" },
            r.smtp_code ? "SMTP " + r.smtp_code : "尚无 SMTP 返回码",
          ),
          r.status === "DELIVERY_UNKNOWN" && r.latest_attempt_id && canWrite()
            ? el(
                "div",
                { class: "actions" },
                button("人工处置", () => resolveRecipient(r)),
              )
            : null,
        ),
      );
  };
  const addEvents = (items) => {
    for (const e of items)
      timeline.append(
        el(
          "li",
          {},
          el("strong", {}, eventNames[e.event_type] || e.event_type),
          el("time", { datetime: e.event_time }, date(e.event_time)),
          el(
            "small",
            {},
            e.source +
              (e.attempt_sequence ? " / 序号 " + e.attempt_sequence : ""),
          ),
        ),
      );
  };
  const addAttempts = (items) => {
    for (const a of items)
      attempts.append(
        el(
          "article",
          { class: "recipient" },
          el(
            "div",
            { class: "recipient-title" },
            el("strong", {}, "尝试 " + a.attempt_number),
            badge(a.result),
          ),
          el("small", { class: "muted" }, date(a.started_at)),
          el("p", { class: "mono" }, a.id),
          a.error_class ? el("small", { class: "muted" }, a.error_class) : null,
        ),
      );
  };
  async function more(kind, page, target, append) {
    if (!page.has_more) return;
    const b = button("加载更多", async () => {
      b.disabled = true;
      try {
        const next = await api(
          `messages/${id}/${kind}?limit=100&after=${encodeURIComponent(page.next_after)}`,
        );
        if (S.generation !== generation) return;
        b.remove();
        append(next.items);
        more(kind, next, target, append);
      } catch (e) {
        b.disabled = false;
        problem(errorText(e));
      }
    });
    target.append(b);
  }
  addRecipients(rs.items);
  addEvents(evs.items);
  addAttempts(ats.items);
  more("recipients", rs, recipients, addRecipients);
  more("attempts", ats, attempts, addAttempts);
  if (!rs.items.length)
    recipients.append(el("p", { class: "muted" }, "暂无收件人记录"));
  if (!ats.items.length)
    attempts.append(el("p", { class: "muted" }, "尚未开始投递尝试"));
  const timeSection = el(
    "section",
    { class: "detail-section" },
    el("h2", {}, "事件时间线"),
    timeline,
  );
  if (!evs.items.length)
    timeSection.append(el("p", { class: "muted" }, "暂无事件"));
  more("events", evs, timeSection, addEvents);
  host.append(
    el(
      "div",
      { class: "detail-layout" },
      el(
        "div",
        { class: "panel" },
        el(
          "section",
          { class: "detail-top" },
          badge(m.status),
          el("h2", {}, m.subject || "（无主题）"),
          el("p", { class: "mono" }, m.id),
          el(
            "p",
            {
              class:
                "callout" + (m.status === "DELIVERY_UNKNOWN" ? " warn" : ""),
            },
            m.status === "DELIVERY_UNKNOWN"
              ? "远端可能已经接收。系统不会自动重发，请先核对收件情况。"
              : m.status === "SANDBOX_STORED"
                ? "这是测试沙箱邮件，系统禁止真实投递，也没有远端接收事实。"
                : "SMTP 接受仅代表远端接受了邮件，不等于收件人已收到。",
          ),
        ),
        el(
          "section",
          { class: "detail-section" },
          facts({
            发件人: m.envelope_from,
            "原 Message-ID": m.message_id || "未提供",
            接收时间: date(m.created_at),
            投递期限: m.expires_at ? date(m.expires_at) : "未设置",
            最早发送时间: m.schedule_at ? date(m.schedule_at) : "立即可投递",
            大小: (m.eml_size / 1024).toFixed(1) + " KiB",
            存档:
              m.archive_state === "AVAILABLE" ? "原文可用" : m.archive_state,
          }),
        ),
        recipients,
        el(
          "section",
          { class: "detail-section" },
          el("h2", {}, "原文与附件"),
          el(
            "p",
            { class: "muted" },
            "此工作台仅显示邮件元数据。正文、附件和外部图片不会在浏览器中加载。",
          ),
        ),
      ),
      el("div", { class: "panel" }, timeSection, attempts),
    ),
  );
}
function access() {
  const host = $("#view");
  host.replaceChildren(
    heading(
      "访问与说明",
      "查看当前身份与操作边界。",
      button("退出登录", logout),
    ),
    el(
      "div",
      { class: "panel" },
      el(
        "section",
        { class: "section-copy" },
        el("h2", {}, "当前会话"),
        facts({
          账户: S.user.id,
          角色: roles[S.user.role],
          会话有效期: "最长 30 分钟；闲置 15 分钟后失效",
          操作审计: "操作者由登录身份确定",
        }),
      ),
      el(
        "section",
        { class: "section-copy" },
        el("h2", {}, "投递状态的含义"),
        el(
          "p",
          {},
          "SMTP 已接受：远端 SMTP 返回接受结果，尚不能据此确认入箱。",
        ),
        el("p", {}, "结果待确认：远端可能已接收，系统不会自动重发。"),
        el(
          "p",
          {},
          "已确认收到：人工根据外部证据作出的记录，不会覆盖原始 SMTP 尝试。",
        ),
      ),
      el(
        "section",
        { class: "section-copy" },
        el("h2", {}, "账户与安全"),
        el(
          "p",
          {},
          "账户由部署管理员配置，退出或服务重启会使当前会话失效。工作台不保存长期管理 API 令牌。",
        ),
        el(
          "p",
          {},
          "SMTP 账户及系统参数通过部署配置和本地管理工具维护。此页面不会伪装成尚未实现的系统设置。",
        ),
      ),
    ),
  );
}
function route(reset = true) {
  if (!S.user) return;
  const parts = location.hash.slice(1).split("/");
  const view = titles[parts[0]] ? parts[0] : "messages";
  if (reset || S.view !== view) {
    S.after = "";
    S.cursors = [];
  }
  S.view = view;
  const generation = ++S.generation;
  problem("");
  document.querySelectorAll("nav a").forEach((a) => {
    a.classList.toggle("active", a.dataset.view === view);
    if (a.dataset.view === view) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  });
  $("#breadcrumb").textContent = titles[view];
  $("#view").replaceChildren(empty("正在读取记录", "请稍候…"));
  let work;
  if (view === "access") {
    access();
    return;
  }
  if (view === "messages" && parts[1])
    work = messageDetail(parts[1], generation);
  else if (view === "feedback")
    work = parts[1]
      ? feedbackDetail(parts[1], generation)
      : feedbackList(generation);
  else if (view === "statistics") work = statisticsView(generation);
  else if (view === "templates") work = templatesView(generation);
  else work = listPage(view, generation);
  work.catch((e) => {
    if (S.generation !== generation) return;
    problem(errorText(e));
    $("#view").replaceChildren(
      heading(
        titles[view],
        "记录暂时无法读取。",
        button(
          view === "statistics" && e.status === 422
            ? "缩短为一小时"
            : "重新加载",
          () => {
            if (view === "statistics" && e.status === 422)
              S.statisticsHours = 1;
            route(false);
          },
        ),
      ),
      el(
        "div",
        { class: "panel" },
        empty(
          "未能加载记录",
          "连接恢复后重新加载。此处不会显示未经确认的结果。",
        ),
      ),
    );
  });
}
window.addEventListener("hashchange", () => route());
request("session")
  .then(signedIn)
  .catch((e) => {
    signedOut();
    if (e.status !== 401) $("#login-error").textContent = errorText(e);
  });
$("#modal").addEventListener("close", () => {
  if (!$("#modal").open) $("#modal").replaceChildren();
});

function feedbackJSON(value) {
  return el("pre", { class: "feedback-data" }, JSON.stringify(value, null, 2));
}
async function feedbackList(generation) {
  const data = await api(
    "feedback?limit=50" +
      (S.after ? "&after=" + encodeURIComponent(S.after) : ""),
  );
  if (S.generation !== generation) return;
  $("#view").replaceChildren(
    heading(
      "反馈待查",
      "收件箱中的反馈仅作为待核对证据，来源未经验证。",
      button("刷新", () => route(false)),
    ),
    el(
      "div",
      { class: "panel" },
      data.items.length
        ? table(
            ["来源", "接收时间", "解析", "状态"],
            data.items.map((x) =>
              el(
                "tr",
                {},
                el(
                  "td",
                  {},
                  el(
                    "a",
                    { href: "#feedback/" + encodeURIComponent(x.id) },
                    x.source,
                  ),
                ),
                el("td", {}, date(x.created_at)),
                el(
                  "td",
                  {},
                  x.parse_state === "PARSED" ? "已解析 · 未验证" : "格式待查",
                ),
                el(
                  "td",
                  {},
                  {
                    QUARANTINED: "待核对",
                    DISMISSED: "已归档处置",
                    CONFLICT: "来源记录冲突",
                  }[x.state] || x.state,
                ),
              ),
            ),
          )
        : empty("还没有反馈记录", "收集或导入的反馈会显示在这里。"),
      pager(data, () => route(false)),
    ),
  );
}
async function feedbackDetail(id, generation) {
  const base = "feedback/" + encodeURIComponent(id);
  const entry = await api(base);
  const publications = await api(base + "/links");
  const publicationPanel = feedbackPublicationPanel(
    entry,
    publications.items,
    generation,
  );
  if (S.generation !== generation) return;
  const explanation = el("div", { "aria-live": "polite" });
  const results = el("div", { "aria-live": "polite" });
  const explain = button("查看格式说明", async () => {
    explain.disabled = true;
    explanation.textContent = "正在核对格式…";
    try {
      const data = await api(
        base +
          "/explanation?" +
          new URLSearchParams({ expected_sha256: entry.sha256 }),
      );
      if (S.generation !== generation) return;
      explanation.replaceChildren(
        el(
          "p",
          {},
          data.strictly_parsed
            ? "严格解析通过，来源仍未经验证。"
            : "严格解析未通过；以下说明仅用于人工排查。",
        ),
        feedbackJSON({ issues: data.issues, blocks: data.blocks }),
      );
    } catch (e) {
      if (S.generation === generation)
        explanation.textContent =
          e.status === 422
            ? "无法提供格式说明：这可能不是支持的退信格式。"
            : errorText(e);
    } finally {
      explain.disabled = false;
    }
  });
  const hint = el("input", {
    id: "feedback-hint",
    required: "",
    maxlength: "998",
    placeholder: "<original@example.test>",
    autocomplete: "off",
  });
  const index = el("input", {
    id: "feedback-index",
    type: "number",
    min: "0",
    max: "99",
    step: "1",
    value: "0",
    required: "",
  });
  const submit = el(
    "button",
    { type: "submit", class: "primary" },
    "查找关联候选",
  );
  const form = el(
    "form",
    {
      class: "feedback-form",
      onsubmit: async (e) => {
        e.preventDefault();
        if (submit.disabled) return;
        submit.disabled = true;
        results.textContent = "正在查找候选…";
        try {
          const data = await api(
            base +
              "/candidates?" +
              new URLSearchParams({
                expected_sha256: entry.sha256,
                rfc_message_id: hint.value,
                report_index: index.value,
                limit: "25",
              }),
          );
          if (S.generation !== generation) return;
          results.replaceChildren(
            ...[
              el(
                "p",
                {},
                `${data.candidates.length} 个候选。候选仅供核对，不能证明反馈真实。`,
              ),
              data.multiple_messages
                ? el(
                    "p",
                    { class: "feedback-warning" },
                    "多个邮件使用相同 Message-ID，无法唯一关联。",
                  )
                : null,
              data.truncated
                ? el(
                    "p",
                    { class: "feedback-warning" },
                    "结果超过 25 条，当前列表不完整。",
                  )
                : null,
              el(
                "details",
                {},
                el("summary", {}, "查询判定依据"),
                feedbackJSON(data.reasons),
              ),
              data.candidates.map((c) =>
                el(
                  "section",
                  { class: "feedback-candidate" },
                  el(
                    "a",
                    {
                      href:
                        "#messages/" + encodeURIComponent(c.request.message_id),
                    },
                    "查看本地邮件记录",
                  ),
                  canAdmin() && !data.multiple_messages && !data.truncated
                    ? button("选择此候选", () => {
                        publicationPanel.select(c.request);
                        publicationPanel.node.scrollIntoView({
                          block: "start",
                        });
                      })
                    : null,
                  facts({
                    当前投递状态:
                      states[c.current_status]?.[0] || c.current_status,
                    本地关联条件: c.assessment.candidate_matches
                      ? "符合 · 仍未验证来源"
                      : "不符合或存在歧义",
                    收件人记录: c.request.recipient_id,
                    投递尝试: c.request.attempt_id,
                  }),
                  el(
                    "details",
                    {},
                    el("summary", {}, "候选判定依据"),
                    feedbackJSON(c.assessment),
                  ),
                ),
              ),
            ]
              .flat()
              .filter((x) => x !== null),
          );
        } catch (e) {
          if (S.generation === generation)
            results.textContent =
              e.status === 422
                ? "无法查询候选，请核对收件人区块编号或稍后重试。"
                : errorText(e);
        } finally {
          submit.disabled = false;
        }
      },
    },
    el("label", { for: "feedback-hint" }, "原始 Message-ID（人工提供）"),
    hint,
    el("label", { for: "feedback-index" }, "收件人区块编号（从 0 开始）"),
    index,
    submit,
  );
  $("#view").replaceChildren(
    heading(
      "反馈详情",
      "核对反馈声称的结果与本地投递事实。",
      el("a", { href: "#feedback" }, "返回反馈列表"),
    ),
    el(
      "div",
      { class: "feedback-warning", role: "note" },
      "来源未经验证 · 自动处置关闭。人工核验结果单独通知网站，不修改 SMTP 投递事实或地址抑制。",
    ),
    publicationPanel.node,
    el(
      "section",
      { class: "panel feedback-panel" },
      el("h2", {}, "保存的证据"),
      facts({
        来源: entry.source,
        记录状态: entry.state,
        解析状态: entry.parse_state,
        接收时间: date(entry.created_at),
        "证据摘要 SHA-256": entry.sha256,
      }),
      el(
        "details",
        {},
        el("summary", {}, "反馈声明（未经验证）"),
        feedbackJSON(entry.report),
      ),
      explain,
      explanation,
    ),
    el(
      "section",
      { class: "panel feedback-panel" },
      el("h2", {}, "关联候选"),
      el(
        "p",
        {},
        "请从原始发件记录提供 Message-ID。即使找到候选，也不会把反馈升级为可信来源。",
      ),
      form,
      results,
    ),
  );
}

async function statisticsView(generation) {
  const hours = Number(S.statisticsHours || 24);
  // Leave the current minute out; this is a submission cohort, not event counts.
  const until = new Date(Date.now() - 60000);
  const from = new Date(until.getTime() - hours * 3600000);
  const data = await api(
    "statistics?" +
      new URLSearchParams({
        from: from.toISOString(),
        until: until.toISOString(),
      }),
  );
  if (S.generation !== generation) return;
  const range = el(
    "select",
    {
      "aria-label": "统计范围",
      value: String(hours),
      onChange: (e) => {
        S.statisticsHours = e.target.value;
        route(false);
      },
    },
    [1, 24, 168, 744].map((n) =>
      el(
        "option",
        { value: String(n), selected: n === hours ? "" : null },
        n === 1 ? "最近 1 小时" : "最近 " + n / 24 + " 天",
      ),
    ),
  );
  const bytes = (n) =>
    (Number.isSafeInteger(n) ? "" : "约 ") +
    Number(n).toLocaleString("zh-CN") +
    " 字节";
  const rows = data.groups.map((g) =>
    el(
      "tr",
      {},
      el(
        "td",
        {},
        g.source === "api"
          ? el(
              "code",
              { class: "statistics-identity" },
              g.api_key_id || "未记录网站身份",
            )
          : g.source === "smtp"
            ? "SMTP 来源合计"
            : "内部来源合计",
      ),
      el("td", {}, g.environment === "sandbox" ? "测试 · 不发信" : "生产"),
      el("td", {}, g.message_count),
      el(
        "td",
        {},
        el(
          "div",
          {},
          Object.entries(g.messages_by_status).map(([state, count]) =>
            el("div", {}, badge(state), " " + count),
          ),
        ),
      ),
      el(
        "td",
        {},
        el(
          "div",
          {},
          Object.entries(g.archives_by_state).map(([state, count]) =>
            el(
              "div",
              {},
              ({
                AVAILABLE: "可读取",
                PURGE_PENDING: "清理中",
                PURGED: "已清理",
              }[state] || state) +
                "：" +
                count +
                " 封 / " +
                bytes(g.logical_raw_bytes_by_archive_state[state] || 0),
            ),
          ),
        ),
      ),
    ),
  );
  $("#view").replaceChildren(
    heading(
      "统计与容量",
      "按网站身份和来源查看提交窗口内邮件的当前状态。",
      range,
      button("刷新", () => route(false)),
    ),
    el(
      "p",
      { class: "callout" },
      "SMTP 已接受不代表已入箱；测试邮件不参与真实投递。原文字节是记录容量，已清理项仍保留历史大小，不代表磁盘占用。",
    ),
    el(
      "p",
      {},
      "提交时间：" +
        date(data.from) +
        " 至 " +
        date(data.until) +
        "（不含结束时刻；排除最近一分钟）",
    ),
    el(
      "p",
      {},
      "快照：" +
        date(data.snapshot_at) +
        " · " +
        data.message_count +
        " 封邮件",
    ),
    rows.length
      ? table(
          [
            "网站身份 / 来源",
            "环境",
            "邮件数",
            "当前状态",
            "原文状态 / 逻辑字节",
          ],
          rows,
        )
      : empty("此范围没有邮件", "可扩大统计范围，或在网站提交邮件后刷新。"),
  );
}

async function templatesView(generation) {
  const identities = await api(
    "template-identities?limit=100&after=" + (S.templateAfter || ""),
  );
  if (generation !== S.generation) return;
  const chosen =
    identities.items.find((k) => k.id === S.templateIdentity) ||
    identities.items[0];
  S.templateIdentity = chosen?.id || "";
  const selector = el(
    "select",
    {
      "aria-label": "网站身份",
      onChange: (e) => {
        S.templateIdentity = e.target.value;
        route(false);
      },
    },
    identities.items.map((k) =>
      el(
        "option",
        { value: k.id, selected: k.id === chosen?.id ? "" : null },
        k.name +
          " / " +
          (k.environment === "sandbox" ? "测试" : "生产") +
          (k.enabled ? "" : " / 已停用"),
      ),
    ),
  );
  const identityNavigation = el(
    "div",
    { class: "actions" },
    S.templateAfter
      ? button("返回首批身份", () => {
          S.templateAfter = "";
          S.templateIdentity = "";
          route(false);
        })
      : null,
    identities.has_more
      ? button("下一批身份", () => {
          S.templateAfter = identities.next_after;
          S.templateIdentity = "";
          route(false);
        })
      : null,
  );
  if (!chosen) {
    $("#view").replaceChildren(
      heading("邮件模板", "草稿经审阅后发布，网站明确选择固定版本。"),
      identityNavigation,
      empty("还没有网站身份", "先通过部署配置添加网站发信身份，再创建模板。"),
    );
    return;
  }
  const base = "templates/" + encodeURIComponent(chosen.id);
  const [drafts, versions] = await Promise.all([
    api(base + "/drafts"),
    api(base + "/versions"),
  ]);
  if (generation !== S.generation) return;
  const edit = (draft) =>
    templateEditor(chosen, draft).catch((e) => problem(errorText(e)));
  const preview = (draft) =>
    templatePreview(chosen, draft).catch((e) => problem(errorText(e)));
  const review = (draft) =>
    templatePublish(chosen, draft).catch((e) => problem(errorText(e)));
  $("#view").replaceChildren(
    ...[
      heading(
        "邮件模板",
        "编辑草稿，核对修订，再发布固定版本。",
        canWrite() && chosen.enabled
          ? button("新建草稿", () => edit(null), "primary")
          : null,
        button("刷新", () => route(false)),
      ),
      el(
        "div",
        { class: "panel template-identity" },
        el("label", {}, "网站身份", selector),
        el("code", {}, chosen.id),
        identityNavigation,
      ),
      el(
        "p",
        { class: "template-activation" },
        "发布和退役需在服务重新加载后生效。这里展示保存记录，不表示运行中的服务已加载该版本。",
      ),
      el(
        "section",
        { class: "panel template-catalog" },
        el("h2", {}, "当前草稿"),
        el(
          "p",
          {},
          "草稿不会发送邮件。编辑保留历史修订，冲突时重新读取并核对。",
        ),
        drafts.items.length
          ? table(
              ["模板", "修订", "内容版本", "保存时间", "操作"],
              drafts.items.map((d) =>
                el(
                  "tr",
                  {},
                  el("td", {}, d.id),

                  el("td", {}, d.revision),
                  el(
                    "td",
                    {},
                    el("code", { title: d.version }, d.version.slice(0, 12)),
                  ),
                  el("td", {}, date(d.saved_at)),
                  el(
                    "td",
                    {},
                    el(
                      "div",
                      { class: "actions" },
                      canWrite() && chosen.enabled
                        ? button("编辑草稿", () => edit(d))
                        : null,
                      canWrite() ? button("预览内容", () => preview(d)) : null,
                      canAdmin() && chosen.enabled
                        ? button("审阅并发布", () => review(d))
                        : null,
                    ),
                  ),
                ),
              ),
            )
          : empty("暂无草稿", "创建第一个草稿，填写主题、正文和变量。"),
      ),
      el(
        "section",
        { class: "panel template-catalog" },
        el("h2", {}, "已发布版本"),
        el("p", {}, "定义与历史不可改写；退役不会改变已入队邮件。"),
        versions.items.length
          ? table(
              ["模板", "语言", "内容版本", "发布记录", "状态", "操作"],
              versions.items.map((v) =>
                el(
                  "tr",
                  {},
                  el("td", {}, v.id),
                  el("td", {}, v.language || "未指定"),
                  el(
                    "td",
                    {},
                    el("code", { title: v.version }, v.version.slice(0, 12)),
                  ),
                  el("td", {}, date(v.published_at)),
                  el("td", {}, v.retired_at ? "已记录退役" : "未退役"),
                  el(
                    "td",
                    {},
                    canAdmin() && !v.retired_at
                      ? button(
                          "退役版本",
                          () => templateRetire(chosen, v),
                          "danger",
                        )
                      : null,
                  ),
                ),
              ),
            )
          : empty("暂无发布版本", "管理员审阅草稿后，可发布一个固定内容版本。"),
      ),
      !canWrite()
        ? el(
            "p",
            { class: "callout" },
            "只读账户只能查看目录元数据，不能读取模板源码或修改内容。",
          )
        : null,
    ].filter(Boolean),
  );
}

async function templateEditor(identity, draft) {
  const generation = S.generation;
  const source = draft
    ? await api(
        `templates/${identity.id}/drafts/${draft.id}/revisions/${draft.revision}`,
      )
    : { id: "", subject: "", text: "", html: "", language: "", variables: [] };
  if (generation !== S.generation) return;
  const error = el("p", { class: "form-error", role: "alert" });
  const save = el("button", { type: "submit", class: "primary" }, "保存草稿");
  const cancel = button("返回模板目录", () => route(false));
  const form = el(
    "form",
    {
      class: "template-editor",
      onSubmit: async (e) => {
        e.preventDefault();
        if (!form.reportValidity()) return;
        const data = new FormData(form);
        save.disabled = true;
        cancel.disabled = true;
        error.textContent = "";
        try {
          const result = await api(`templates/${identity.id}/drafts`, {
            expected_revision: draft?.revision || 0,
            template: {
              id: data.get("id"),
              language: data.get("language"),
              subject: data.get("subject"),
              text: data.get("text"),
              html: data.get("html"),
              variables: data
                .get("variables")
                .split(",")
                .map((x) => x.trim())
                .filter(Boolean),
            },
            reason: data.get("reason"),
          });
          if (generation !== S.generation) return;
          route(false);
          problem(`草稿修订 ${result.revision} 已保存，尚未发布。`, "success");
        } catch (e) {
          if (generation === S.generation) error.textContent = errorText(e);
        } finally {
          save.disabled = false;
          cancel.disabled = false;
        }
      },
    },
    el(
      "div",
      { class: "template-editor-grid" },
      el(
        "section",
        { class: "template-content" },
        field("邮件主题（可选）", "subject", source.subject, "text", {
          maxlength: 256,
          optional: true,
        }),
        field("纯文本正文", "text", source.text, "textarea", {
          maxlength: 16384,
          optional: true,
          rows: 10,
        }),
        field("HTML 正文源码", "html", source.html, "textarea", {
          maxlength: 16384,
          optional: true,
          rows: 10,
        }),
      ),
      el(
        "aside",
        { class: "template-settings" },
        el("h2", {}, "这份草稿"),
        el("p", {}, identity.name),
        field("模板名称", "id", source.id, "text", {
          maxlength: 32,
          pattern: "[a-z][a-z0-9_]{0,31}",
          readonly: draft ? "" : null,
        }),
        field("语言（可选）", "language", source.language || "", "text", {
          maxlength: 35,
          optional: true,
        }),
        field(
          "变量名称（逗号分隔）",
          "variables",
          source.variables.join(","),
          "text",
          { optional: true },
        ),
        el(
          "p",
          { class: "muted" },
          "在正文中用 {{变量名}} 插入变量。每个声明的变量都需要使用；至少填写一种正文。",
        ),
        field("处理理由", "reason", "", "textarea", { maxlength: 256 }),
        el(
          "p",
          { class: "muted" },
          draft
            ? `基于修订 ${draft.revision} 编辑。保存会生成新的历史修订。`
            : "保存后由管理员审阅发布，草稿本身不会发信。",
        ),
      ),
    ),
    error,
    el("div", { class: "template-editor-actions" }, cancel, save),
  );
  $("#view").replaceChildren(
    heading(
      draft ? "编辑模板草稿" : "新建模板草稿",
      "保存内容，发布前再核对固定修订。",
    ),
    form,
  );
  form.querySelector('[name="subject"]').focus();
}

async function templatePublish(identity, draft) {
  const generation = S.generation;
  const source = await api(
    `templates/${identity.id}/drafts/${draft.id}/revisions/${draft.revision}`,
  );
  if (generation !== S.generation) return;
  dialog(
    "审阅并发布模板",
    el(
      "div",
      {},
      summary(source.id, source.version),
      facts({
        网站身份: identity.name,
        修订: draft.revision,
        语言: source.language || "未指定",
      }),
      el("h3", {}, "邮件主题"),
      el("pre", { class: "template-source" }, source.subject),
      el("h3", {}, "纯文本正文"),
      el("pre", { class: "template-source" }, source.text),
      el("h3", {}, "HTML 正文源码"),
      el("pre", { class: "template-source" }, source.html),
      el("p", {}, "变量：" + source.variables.join(", ")),
      el(
        "label",
        { class: "check-label" },
        el("input", { type: "checkbox", name: "reviewed", required: "" }),
        "我已核对这个修订的内容与变量。",
      ),
      field("处理理由", "reason", "", "textarea", { maxlength: 256 }),
    ),
    "发布此修订",
    (data) =>
      api(`templates/${identity.id}/drafts/${draft.id}/publish`, {
        revision: draft.revision,
        version: source.version,
        reason: data.get("reason"),
      }).then((result) => ({
        ...result,
        notice: "模板版本已发布，仍需明确启用并重新加载服务。",
      })),
    "内容按源码展示，不加载外部资源。若草稿已变化，发布会拒绝；成功发布后仍需明确启用并重新加载。",
  );
}

function templateRetire(identity, version) {
  dialog(
    "退役模板版本",
    el(
      "div",
      {},
      summary(version.id, version.version),
      el(
        "p",
        { class: "callout warn" },
        "退役不可撤销。重新加载后，旧请求即使带原幂等键也不能再使用这个版本。已入队邮件不受影响。",
      ),
      el(
        "label",
        { class: "check-label" },
        el("input", { type: "checkbox", name: "retry_window", required: "" }),
        "我已确认业务网站的请求重试窗口结束。",
      ),
      field("处理理由", "reason", "", "textarea", { maxlength: 256 }),
    ),
    "记录退役",
    (data) =>
      api(
        `templates/${identity.id}/versions/${version.id}/${version.version}/retire`,
        {
          reason: data.get("reason"),
          acknowledge_retry_window: data.has("retry_window"),
        },
      ).then((result) => ({
        ...result,
        notice: "已记录版本退役，重新加载服务后生效。",
      })),
    "此操作记录退役，需要重新加载服务后生效。",
  );
}

// Parse into an inert template, then reconstruct a closed vocabulary. No URLs,
// attributes, CSS, foreign namespaces or active elements reach the frame.
function templatePreviewDocument(html) {
  const inert = document.createElement("template");
  inert.innerHTML = html;
  const container = document.createElement("div");
  const allowed = new Set(
    "p div span h1 h2 h3 h4 h5 h6 strong b em i u s small sub sup br hr pre code blockquote ul ol li table caption thead tbody tfoot tr td th".split(
      " ",
    ),
  );
  const dropped = new Set(
    "script style iframe object embed svg math template noscript head meta link base form input button select textarea video audio source".split(
      " ",
    ),
  );
  let count = 0;
  function copy(node, target, depth) {
    if (++count > 4096 || depth > 64) return;
    if (node.nodeType === Node.TEXT_NODE) {
      target.append(document.createTextNode(node.textContent));
      return;
    }
    if (
      node.nodeType !== Node.ELEMENT_NODE ||
      node.namespaceURI !== "http://www.w3.org/1999/xhtml"
    )
      return;
    const name = node.localName;
    if (dropped.has(name)) return;
    if (name === "img") {
      target.append(document.createTextNode("[图片已隐藏]"));
      return;
    }
    const next = allowed.has(name) ? document.createElement(name) : target;
    if (next !== target) target.append(next);
    for (const child of node.childNodes) copy(child, next, depth + 1);
  }
  for (const node of inert.content.childNodes) copy(node, container, 0);
  return (
    "<!doctype html><html><head><meta charset=\"utf-8\"><meta http-equiv=\"Content-Security-Policy\" content=\"default-src 'none'; base-uri 'none'; form-action 'none'\"></head><body>" +
    container.innerHTML +
    "</body></html>"
  );
}

async function templatePreview(identity, draft) {
  const generation = S.generation;
  const source = await api(
    `templates/${identity.id}/drafts/${draft.id}/revisions/${draft.revision}`,
  );
  if (generation !== S.generation) return;
  const results = el("section", {
    class: "template-preview-result",
    "aria-live": "polite",
  });
  const error = el("p", { class: "form-error", role: "alert" });
  const render = el("button", { type: "submit", class: "primary" }, "生成预览");
  const publish = button(
    "审阅并发布这个修订",
    () => templatePublish(identity, draft).catch((e) => problem(errorText(e))),
    "primary",
  );
  publish.disabled = true;
  let requestNumber = 0;
  const form = el(
    "form",
    {
      class: "template-preview-form",
      onInput: () => {
        requestNumber++;
        results.replaceChildren();
        publish.disabled = true;
        error.textContent = "";
      },
      onSubmit: async (e) => {
        e.preventDefault();
        if (!form.reportValidity()) return;
        const number = ++requestNumber;
        results.replaceChildren();
        publish.disabled = true;
        error.textContent = "";
        render.disabled = true;
        const values = Object.fromEntries(new FormData(form));
        try {
          const value = await api(
            `templates/${identity.id}/drafts/${draft.id}/preview`,
            {
              revision: draft.revision,
              version: source.version,
              language: source.language || "",
              variables: values,
            },
          );
          if (generation !== S.generation || number !== requestNumber) return;
          const frame = el("iframe", {
            title: "隔离 HTML 预览",
            sandbox: "",
            referrerpolicy: "no-referrer",
            class: "template-preview-frame",
          });
          frame.srcdoc = templatePreviewDocument(value.html);
          results.replaceChildren(
            el("h2", {}, "渲染结果"),
            el(
              "p",
              { class: "muted" },
              `修订 ${value.revision} · 渲染内容 ${value.rendered_bytes} 字节`,
            ),
            el("h3", {}, "主题"),
            el("p", {}, value.subject || "（无主题）"),
            el("h3", {}, "纯文本"),
            el(
              "pre",
              { class: "template-source" },
              value.text || "（无纯文本）",
            ),
            el("h3", {}, "HTML 结构预览"),
            value.html ? frame : el("p", {}, "（无 HTML）"),
          );
          publish.disabled = false;
        } catch (e) {
          if (generation === S.generation && number === requestNumber)
            error.textContent = errorText(e);
        } finally {
          render.disabled = false;
        }
      },
    },
    el("h2", {}, "示例变量"),
    el(
      "p",
      { class: "muted" },
      "只用于本次预览，不保存、不发送。修改变量后需要重新生成。",
    ),
    ...source.variables.map((name) =>
      field(name, name, "", "text", { maxlength: 1024 }),
    ),
    error,
    render,
  );
  $("#view").replaceChildren(
    heading(
      "预览模板",
      `${identity.name} / ${source.id} / 修订 ${draft.revision}`,
      button("返回模板目录", () => route(false)),
    ),
    facts({ 内容版本: source.version, 语言: source.language || "未指定" }),
    el(
      "p",
      { class: "callout" },
      "受限预览保留文字和基本结构，移除图片、链接、样式及活动内容。它不是邮件客户端的最终显示效果，也不会发送邮件。",
    ),
    form,
    results,
    ...(canAdmin() && identity.enabled ? [publish] : []),
  );
}

function feedbackPublicationPanel(entry, items, generation) {
  const labels = {
    PENDING_REVIEW: "待人工核验",
    FAILURE_CONFIRMED_BY_OPERATOR: "人工确认失败 · 来源未验证",
    WITHDRAWN: "已撤回",
    EVIDENCE_CONFLICT: "证据冲突 · 结论失效",
  };
  const node = el(
    "section",
    { class: "panel feedback-panel" },
    el("h2", {}, "通知业务网站"),
    el(
      "p",
      {},
      "先关联到已被 SMTP 接受的业务邮件，再人工确认失败。网站收到独立反馈记录；这不是 Provider 来源认证，也不证明地址无效。",
    ),
  );
  const fields = {};
  const status = el("p", { "aria-live": "polite" });
  for (const item of items) {
    const section = el(
      "section",
      { class: "feedback-candidate" },
      facts({
        核验状态: labels[item.state],
        收件人记录: item.recipient_id,
        投递尝试: item.attempt_id,
        版本: item.revision,
      }),
    );
    if (canAdmin() && item.revision < 2 && item.state !== "WITHDRAWN") {
      const reason = el("textarea", {
        required: "",
        maxlength: "2048",
        "aria-label": "核验理由",
      });
      const ack = el("input", { type: "checkbox", required: "" });
      const action = el(
        "select",
        { "aria-label": "核验操作" },
        ...(item.state === "PENDING_REVIEW"
          ? [el("option", { value: "CONFIRM_FAILURE" }, "人工确认失败")]
          : []),
        el("option", { value: "WITHDRAW" }, "撤回关联或结论"),
      );
      const submit = el("button", { type: "submit" }, "保存核验结果");
      section.append(
        el(
          "form",
          {
            class: "feedback-form",
            onsubmit: async (e) => {
              e.preventDefault();
              if (submit.disabled) return;
              submit.disabled = true;
              try {
                await api("feedback-links/" + item.id + "/reviews", {
                  expected_revision: item.revision,
                  action: action.value,
                  reason: reason.value,
                  acknowledge_unverified: ack.checked,
                });
                if (S.generation === generation)
                  await feedbackDetail(entry.id, generation);
              } catch (error) {
                status.textContent =
                  errorText(error) + " 请刷新核对，勿重复确认。";
              } finally {
                submit.disabled = false;
              }
            },
          },
          action,
          el("label", {}, "核验理由", reason),
          el(
            "label",
            { class: "feedback-ack" },
            ack,
            " 我知道来源仍未验证，且此操作会通知网站",
          ),
          submit,
        ),
      );
    }
    node.append(section);
  }
  if (canAdmin()) {
    const form = el("form", { class: "feedback-form" });
    for (const [key, label] of Object.entries({
      message_id: "邮件 Gateway ID",
      provider_id: "Provider ID",
      attempt_id: "投递尝试 ID",
      recipient_id: "收件人记录 ID",
      report_index: "收件人区块编号",
    })) {
      fields[key] = el("input", {
        required: "",
        maxlength: "36",
        "aria-label": label,
        ...(key === "report_index"
          ? { type: "number", min: "0", max: "99", value: "0" }
          : {}),
      });
      form.append(el("label", {}, label, fields[key]));
    }
    const manual = el("input", { type: "checkbox" });
    const ack = el("input", { type: "checkbox", required: "" });
    const reason = el("textarea", {
      required: "",
      maxlength: "2048",
      "aria-label": "关联理由",
    });
    const submit = el(
      "button",
      { type: "submit", class: "primary" },
      "关联并通知网站待核验",
    );
    form.append(
      el("label", {}, "关联理由", reason),
      ...(entry.parse_state === "UNSUPPORTED"
        ? [
            el(
              "label",
              { class: "feedback-ack" },
              manual,
              " 格式不标准，我已手工核对收件人和原始邮件（仅使用区块 0）",
            ),
          ]
        : []),
      el(
        "label",
        { class: "feedback-ack" },
        ack,
        " 我知道关联由人工提供，来源未经验证",
      ),
      submit,
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      if (submit.disabled) return;
      submit.disabled = true;
      const body = {
        feedback_id: entry.id,
        expected_sha256: entry.sha256,
        reason: reason.value,
        acknowledge_unverified: ack.checked,
        manual_association: manual.checked,
      };
      for (const [key, input] of Object.entries(fields))
        body[key] = key === "report_index" ? Number(input.value) : input.value;
      try {
        await api("feedback/" + entry.id + "/links", body);
        if (S.generation === generation)
          await feedbackDetail(entry.id, generation);
      } catch (error) {
        status.textContent =
          errorText(error) + " 请刷新核对关联条件，勿盲目重试。";
      } finally {
        submit.disabled = false;
      }
    });
    node.append(el("details", {}, el("summary", {}, "新建人工关联"), form));
  }
  node.append(status);
  return {
    node,
    select(request) {
      for (const [key, input] of Object.entries(fields))
        input.value = request[key];
      const detail = node.querySelector("details");
      if (detail) detail.open = true;
    },
  };
}
