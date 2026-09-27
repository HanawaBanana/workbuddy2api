const core = require('@actions/core');

/**
 * workflow_dispatch 人工重跑入口的目标解析。
 *
 * 背景：ai-governance.yml 一直声明着 workflow_dispatch，但 index.js 只在 issues /
 * pull_request_target 事件下进入处理分支 —— dispatch 事件里 context.payload.issue 与
 * context.payload.pull_request 都是 undefined，于是每次手动触发都落到「事件类型不匹配，
 * 跳过处理」，退出码 0、界面绿勾。维护者会读成「治理已经跑过且没有需要处理的内容」，
 * 而「AI 后端故障恢复后补跑」「AI 误判后人工纠偏」恰恰都依赖这个入口。
 *
 * 这里只做两件事：把输入解析成目标编号（非法 / 缺失 / 冲突各有明确行为），并按编号取回
 * 目标对象；真正的治理仍复用 handleNewIssue / handleNewPR，不复制任何链路。
 */

/**
 * 解析单个目标编号：空值 → null（未指定），非正整数 → 抛错（不静默跳过）。
 * @param {string|number|undefined} raw 输入原值
 * @param {string} inputName 输入名，用于报错定位
 * @returns {number|null}
 */
function parseTargetNumber(raw, inputName) {
  const value = String(raw === undefined || raw === null ? '' : raw).trim();
  if (value === '') {
    return null;
  }
  if (!/^\d+$/.test(value)) {
    throw new Error(`${inputName} 必须是正整数编号，实际收到：${value}`);
  }
  const number = parseInt(value, 10);
  if (!Number.isFinite(number) || number <= 0) {
    throw new Error(`${inputName} 必须是正整数编号，实际收到：${value}`);
  }
  return number;
}

/**
 * 按输入取回重跑目标。
 *
 * 行为约定：
 *   - 两个输入都为空 → 返回 null（调用方据此告警，不再静默空转）；
 *   - 两个都填 → 抛错（不做任何猜测，避免误治理另一条内容）；
 *   - issueNumber 填了 PR 编号（GitHub 的 issues.get 对 PR 同样返回数据，带 pull_request
 *     字段）→ 改走 PR 链路，避免把 PR 当 issue 评论/关闭。
 *
 * @param {Object} octokit GitHub 客户端
 * @param {string} owner
 * @param {string} repo
 * @param {{issueNumber?: string, prNumber?: string}} inputs
 * @returns {Promise<{kind: 'issue'|'pr', number: number, target: Object}|null>}
 */
async function loadDispatchTarget(octokit, owner, repo, inputs = {}) {
  const issueNumber = parseTargetNumber(inputs.issueNumber, 'issue-number');
  const prNumber = parseTargetNumber(inputs.prNumber, 'pr-number');

  if (issueNumber && prNumber) {
    throw new Error('workflow_dispatch 只接受一个目标：issue-number 与 pr-number 不能同时填写');
  }
  if (!issueNumber && !prNumber) {
    return null;
  }

  if (prNumber) {
    const { data } = await octokit.rest.pulls.get({ owner, repo, pull_number: prNumber });
    core.info(`workflow_dispatch 目标：PR #${prNumber}（${data.state}）`);
    return { kind: 'pr', number: prNumber, target: data };
  }

  const { data } = await octokit.rest.issues.get({ owner, repo, issue_number: issueNumber });
  if (data && data.pull_request) {
    const { data: pr } = await octokit.rest.pulls.get({ owner, repo, pull_number: issueNumber });
    core.warning(`issue-number=${issueNumber} 实际是 Pull Request，已改走 PR 治理链路`);
    return { kind: 'pr', number: issueNumber, target: pr };
  }
  core.info(`workflow_dispatch 目标：Issue #${issueNumber}（${data.state}）`);
  return { kind: 'issue', number: issueNumber, target: data };
}

/**
 * 读取 workflow_dispatch 的重跑输入。
 *
 * 环境变量命名陷阱：@actions/core 的 getInput('issue-number') 找的是 `INPUT_ISSUE-NUMBER`
 * （只把空格换成下划线，连字符原样保留），而 action.yml 里声明的 env 是 `INPUT_ISSUE_NUMBER`
 * —— 直接 getInput 会永远拿到空串（2026-09 实测：手动触发填了编号仍落到「未指定目标」）。
 * 本仓库既有输入（canonical-label / skip-users / analyze-file-changes 等）都靠
 * `|| process.env.INPUT_*` 回落，这里沿用同一约定。
 *
 * @param {Object} env 环境变量表（默认 process.env，测试可注入）
 * @param {Function} getInput core.getInput（测试可注入）
 * @returns {{issueNumber: string, prNumber: string}}
 */
function readDispatchInputs(env = process.env, getInput = core.getInput) {
  return {
    issueNumber: getInput('issue-number') || env.INPUT_ISSUE_NUMBER || '',
    prNumber: getInput('pr-number') || env.INPUT_PR_NUMBER || ''
  };
}

module.exports = {
  parseTargetNumber,
  loadDispatchTarget,
  readDispatchInputs
};
