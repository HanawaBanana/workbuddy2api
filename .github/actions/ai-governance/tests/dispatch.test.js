const { parseTargetNumber, loadDispatchTarget, readDispatchInputs } = require('../src/utils/dispatch');

describe('parseTargetNumber', () => {
  test('空值返回 null（未指定目标）', () => {
    expect(parseTargetNumber('', 'issue-number')).toBeNull();
    expect(parseTargetNumber(undefined, 'issue-number')).toBeNull();
    expect(parseTargetNumber('   ', 'issue-number')).toBeNull();
  });

  test('解析正整数', () => {
    expect(parseTargetNumber('42', 'issue-number')).toBe(42);
    expect(parseTargetNumber(7, 'pr-number')).toBe(7);
  });

  test('非数字 / 非正数直接报错，不静默跳过', () => {
    expect(() => parseTargetNumber('abc', 'issue-number')).toThrow('issue-number');
    expect(() => parseTargetNumber('0', 'issue-number')).toThrow('issue-number');
    expect(() => parseTargetNumber('-3', 'pr-number')).toThrow('pr-number');
    expect(() => parseTargetNumber('3.5', 'pr-number')).toThrow('pr-number');
  });
});

describe('loadDispatchTarget', () => {
  const makeOctokit = ({ issue = {}, pr = {} } = {}) => ({
    rest: {
      issues: { get: jest.fn().mockResolvedValue({ data: issue }) },
      pulls: { get: jest.fn().mockResolvedValue({ data: pr }) }
    }
  });

  test('未指定目标返回 null，且不发起任何 API 调用', async () => {
    const octokit = makeOctokit();
    await expect(loadDispatchTarget(octokit, 'o', 'r', {})).resolves.toBeNull();
    expect(octokit.rest.issues.get).not.toHaveBeenCalled();
    expect(octokit.rest.pulls.get).not.toHaveBeenCalled();
  });

  test('issue-number 走 issues.get，返回 issue 目标', async () => {
    const octokit = makeOctokit({ issue: { number: 4, state: 'open', title: 't', user: { login: 'someone' } } });
    await expect(loadDispatchTarget(octokit, 'o', 'r', { issueNumber: '4' }))
      .resolves.toMatchObject({ kind: 'issue', number: 4 });
    expect(octokit.rest.issues.get).toHaveBeenCalledWith({ owner: 'o', repo: 'r', issue_number: 4 });
    expect(octokit.rest.pulls.get).not.toHaveBeenCalled();
  });

  test('pr-number 走 pulls.get，返回 pr 目标', async () => {
    const octokit = makeOctokit({ pr: { number: 3, state: 'open', title: 'p', user: { login: 'someone' } } });
    await expect(loadDispatchTarget(octokit, 'o', 'r', { prNumber: '3' }))
      .resolves.toMatchObject({ kind: 'pr', number: 3 });
    expect(octokit.rest.pulls.get).toHaveBeenCalledWith({ owner: 'o', repo: 'r', pull_number: 3 });
    expect(octokit.rest.issues.get).not.toHaveBeenCalled();
  });

  test('issue-number 填了 PR 编号：凭 pull_request 字段改走 PR 链路', async () => {
    const octokit = makeOctokit({
      issue: { number: 3, pull_request: { url: 'https://api.github.com/repos/o/r/pulls/3' } },
      pr: { number: 3, state: 'open', title: 'p', user: { login: 'someone' } }
    });
    await expect(loadDispatchTarget(octokit, 'o', 'r', { issueNumber: '3' }))
      .resolves.toMatchObject({ kind: 'pr', number: 3 });
    expect(octokit.rest.pulls.get).toHaveBeenCalledWith({ owner: 'o', repo: 'r', pull_number: 3 });
  });

  test('同时指定两个目标直接报错（不做任何猜测）', async () => {
    const octokit = makeOctokit();
    await expect(loadDispatchTarget(octokit, 'o', 'r', { issueNumber: '1', prNumber: '2' }))
      .rejects.toThrow('只接受一个目标');
  });
});

describe('readDispatchInputs', () => {
  test('getInput 取不到时回落到 INPUT_*_NUMBER 环境变量（连字符输入名的实际通道）', () => {
    const env = { INPUT_ISSUE_NUMBER: '4', INPUT_PR_NUMBER: '' };
    expect(readDispatchInputs(env, () => '')).toEqual({ issueNumber: '4', prNumber: '' });
  });

  test('getInput 有值时优先于环境变量', () => {
    const env = { INPUT_ISSUE_NUMBER: '4', INPUT_PR_NUMBER: '9' };
    const getInput = name => (name === 'pr-number' ? '3' : '');
    expect(readDispatchInputs(env, getInput)).toEqual({ issueNumber: '4', prNumber: '3' });
  });

  test('两者都空时返回空串（交由 loadDispatchTarget 判为未指定目标）', () => {
    expect(readDispatchInputs({}, () => '')).toEqual({ issueNumber: '', prNumber: '' });
  });
});
